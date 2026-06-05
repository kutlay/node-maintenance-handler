/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package poller implements the Linode maintenance polling loop.
// It polls the Linode account-maintenance endpoint on a fixed interval and
// creates, updates, or completes NodeMaintenance CRDs to reflect current state.
package poller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	maintenancev1 "linode.com/node-maintenance-controller/api/v1"
	linodeclient "linode.com/node-maintenance-controller/internal/linode"
)

var log = logf.Log.WithName("poller")

// Config holds configuration for the LinodeMaintenancePoller.
type Config struct {
	// SecretName is the name of the Kubernetes Secret containing the Linode API token.
	SecretName string

	// SecretNamespace is the namespace of the Secret.
	SecretNamespace string

	// SecretKey is the key within the Secret data that holds the token value.
	SecretKey string

	// PollInterval is how often the Linode API is queried.
	PollInterval time.Duration

	// MaintenanceWindow is the look-ahead duration: nodes whose maintenance is
	// scheduled within this duration are given NodeMaintenance objects.
	MaintenanceWindow time.Duration

	// APIEndpoint overrides the Linode API base URL when non-empty
	APIEndpoint string
}

// LinodeMaintenancePoller polls the Linode maintenance API and manages
// NodeMaintenance CRDs. It implements sigs.k8s.io/controller-runtime/pkg/manager.Runnable.
type LinodeMaintenancePoller struct {
	client client.Client
	// reader is a non-cached API reader used exclusively for Secret reads so that
	// controller-runtime does not start a Secret informer (which would require
	// list+watch RBAC and block WaitForCacheSync).
	reader client.Reader
	cfg    Config
}

// New creates a LinodeMaintenancePoller. reader must be a non-cached reader
// (mgr.GetAPIReader()) so that Secret reads bypass the informer cache.
func New(c client.Client, reader client.Reader, cfg Config) *LinodeMaintenancePoller {
	return &LinodeMaintenancePoller{client: c, reader: reader, cfg: cfg}
}

// Start implements manager.Runnable. It runs the poll loop until ctx is cancelled.
func (p *LinodeMaintenancePoller) Start(ctx context.Context) error {
	log.Info("Starting Linode maintenance poller",
		"pollInterval", p.cfg.PollInterval,
		"maintenanceWindow", p.cfg.MaintenanceWindow,
	)

	// Run immediately on startup so we don't have to wait one full interval.
	p.reconcileAll(ctx)

	ticker := time.NewTicker(p.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.reconcileAll(ctx)
		case <-ctx.Done():
			log.Info("Stopping Linode maintenance poller")
			return nil
		}
	}
}

// reconcileAll fetches all Linode maintenances and syncs the NodeMaintenance CRDs.
// Errors are logged but do not abort the loop so that a single bad cycle does not
// prevent the next one from running.
func (p *LinodeMaintenancePoller) reconcileAll(ctx context.Context) {
	log.Info("Starting poll cycle",
		"maintenanceWindow", p.cfg.MaintenanceWindow,
	)

	token, err := p.readToken(ctx)
	if err != nil {
		log.Error(err, "Could not read Linode token from Secret",
			"secret", fmt.Sprintf("%s/%s", p.cfg.SecretNamespace, p.cfg.SecretName),
		)
		return
	}

	lc := linodeclient.NewClient(token, p.cfg.APIEndpoint)
	maintenances, err := lc.ListMaintenances(ctx)
	if err != nil {
		log.Error(err, "Could not list Linode maintenances")
		return
	}

	log.Info("Received Linode maintenance results", "count", len(maintenances))
	for _, m := range maintenances {
		log.Info("Linode maintenance entry",
			"linodeID", m.LinodeID,
			"label", m.Label,
			"type", m.MaintenanceType,
			"scheduledAt", m.ScheduledAt.UTC().Format(time.RFC3339),
		)
	}

	// Build a LinodeID → Maintenance lookup map.
	maintenanceByID := make(map[int64]linodeclient.Maintenance, len(maintenances))
	for _, m := range maintenances {
		maintenanceByID[m.LinodeID] = m
	}

	// List all nodes in the cluster.
	nodeList := &corev1.NodeList{}
	if err := p.client.List(ctx, nodeList); err != nil {
		log.Error(err, "Could not list Nodes")
		return
	}

	log.Info("Evaluating nodes against maintenance window",
		"nodeCount", len(nodeList.Items),
		"maintenanceWindow", p.cfg.MaintenanceWindow,
	)

	now := time.Now()

	for i := range nodeList.Items {
		node := &nodeList.Items[i]

		linodeID, parseErr := parseLinodeID(node.Spec.ProviderID)
		if parseErr != nil {
			log.V(1).Info("Skipping Node without parseable Linode provider ID",
				"node", node.Name, "providerID", node.Spec.ProviderID,
			)
			continue
		}

		m, found := maintenanceByID[linodeID]

		// A node is "within the maintenance window" if its scheduled time is
		// in the past (already active) or within the look-ahead duration.
		withinWindow := found &&
			(m.ScheduledAt.Before(now) || m.ScheduledAt.Sub(now) <= p.cfg.MaintenanceWindow)

		if !found {
			log.Info("Node has no upcoming Linode maintenance", "node", node.Name, "linodeID", linodeID)
		} else if withinWindow {
			log.Info("Node is within maintenance window — will ensure NodeMaintenance",
				"node", node.Name,
				"linodeID", linodeID,
				"scheduledAt", m.ScheduledAt.UTC().Format(time.RFC3339),
				"timeUntilMaintenance", time.Until(m.ScheduledAt).Truncate(time.Second).String(),
			)
		} else {
			log.Info("Node has maintenance scheduled outside window — no action needed",
				"node", node.Name,
				"linodeID", linodeID,
				"scheduledAt", m.ScheduledAt.UTC().Format(time.RFC3339),
				"timeUntilMaintenance", time.Until(m.ScheduledAt).Truncate(time.Second).String(),
				"maintenanceWindow", p.cfg.MaintenanceWindow,
			)
		}

		if withinWindow {
			if syncErr := p.ensureNodeMaintenance(ctx, node, m); syncErr != nil {
				log.Error(syncErr, "Could not ensure NodeMaintenance", "node", node.Name)
			}
		} else {
			if syncErr := p.setCompletedIfExists(ctx, node.Name); syncErr != nil {
				log.Error(syncErr, "Could not mark NodeMaintenance as Completed", "node", node.Name)
			}
		}
	}

	log.Info("Poll cycle complete")
}

// ensureNodeMaintenance creates or updates the NodeMaintenance CRD for node.
func (p *LinodeMaintenancePoller) ensureNodeMaintenance(
	ctx context.Context,
	node *corev1.Node,
	m linodeclient.Maintenance,
) error {
	now := metav1.Now()

	phase := maintenancev1.MaintenancePhasePending
	if m.ScheduledAt.Before(time.Now()) {
		phase = maintenancev1.MaintenancePhaseActive
	}

	existing := &maintenancev1.NodeMaintenance{}
	err := p.client.Get(ctx, types.NamespacedName{Name: node.Name}, existing)

	if errors.IsNotFound(err) {
		// Create a new NodeMaintenance.
		nm := p.buildNodeMaintenance(node, m)
		if createErr := p.client.Create(ctx, nm); createErr != nil {
			return fmt.Errorf("creating NodeMaintenance for node %s: %w", node.Name, createErr)
		}
		log.Info("Created NodeMaintenance", "node", node.Name, "scheduledAt", m.ScheduledAt)

		// Set the initial status. We must re-fetch after Create to get the
		// server-assigned resourceVersion before patching status.
		created := &maintenancev1.NodeMaintenance{}
		if getErr := p.client.Get(ctx, types.NamespacedName{Name: node.Name}, created); getErr != nil {
			return fmt.Errorf("re-fetching NodeMaintenance after create: %w", getErr)
		}
		created.Status.Phase = phase
		created.Status.LastSyncTime = &now
		if statusErr := p.client.Status().Update(ctx, created); statusErr != nil {
			return fmt.Errorf("setting initial NodeMaintenance status: %w", statusErr)
		}
		return nil
	}

	if err != nil {
		return fmt.Errorf("getting NodeMaintenance for node %s: %w", node.Name, err)
	}

	// Already exists: don't overwrite Completed (the reconciler is cleaning up).
	if existing.Status.Phase == maintenancev1.MaintenancePhaseCompleted {
		return nil
	}

	// Update phase and bump LastSyncTime (the heartbeat triggers the reconciler
	// to re-apply any signals that may have been removed externally).
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Status.Phase = phase
	existing.Status.LastSyncTime = &now
	if patchErr := p.client.Status().Patch(ctx, existing, patch); patchErr != nil {
		return fmt.Errorf("patching NodeMaintenance status for node %s: %w", node.Name, patchErr)
	}
	return nil
}

// setCompletedIfExists transitions an existing NodeMaintenance to Completed,
// signalling the reconciler to remove node signals and clean up the object.
func (p *LinodeMaintenancePoller) setCompletedIfExists(ctx context.Context, nodeName string) error {
	nm := &maintenancev1.NodeMaintenance{}
	err := p.client.Get(ctx, types.NamespacedName{Name: nodeName}, nm)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting NodeMaintenance for node %s: %w", nodeName, err)
	}

	if nm.Status.Phase == maintenancev1.MaintenancePhaseCompleted {
		// Already in the terminal phase; the reconciler is handling cleanup.
		return nil
	}

	now := metav1.Now()
	patch := client.MergeFrom(nm.DeepCopy())
	nm.Status.Phase = maintenancev1.MaintenancePhaseCompleted
	nm.Status.LastSyncTime = &now
	if patchErr := p.client.Status().Patch(ctx, nm, patch); patchErr != nil {
		return fmt.Errorf("patching NodeMaintenance to Completed for node %s: %w", nodeName, patchErr)
	}
	log.Info("Marked NodeMaintenance as Completed", "node", nodeName)
	return nil
}

// buildNodeMaintenance constructs a new NodeMaintenance object.
// The ownerReference to the Node enables Kubernetes GC to delete the
// NodeMaintenance automatically when the Node is deleted.
func (p *LinodeMaintenancePoller) buildNodeMaintenance(
	node *corev1.Node,
	m linodeclient.Maintenance,
) *maintenancev1.NodeMaintenance {
	window := metav1.Duration{Duration: p.cfg.MaintenanceWindow}

	return &maintenancev1.NodeMaintenance{
		ObjectMeta: metav1.ObjectMeta{
			Name: node.Name,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "v1",
					Kind:       "Node",
					Name:       node.Name,
					UID:        node.UID,
					// BlockOwnerDeletion is omitted (false) so that NodeMaintenance
					// does not delay node deletion in any Kubernetes GC scenario.
				},
			},
		},
		Spec: maintenancev1.NodeMaintenanceSpec{
			NodeName:        node.Name,
			LinodeID:        m.LinodeID,
			ScheduledAt:     metav1.Time{Time: m.ScheduledAt},
			MaintenanceType: m.MaintenanceType,
			Entity: &maintenancev1.LinodeEntity{
				ID:    m.LinodeID,
				Label: m.Label,
				Type:  "linode",
				URL:   m.EntityURL,
			},
			WasSchedulable:    !node.Spec.Unschedulable,
			MaintenanceWindow: &window,
		},
	}
}

// readToken reads the Linode API token from the configured Kubernetes Secret.
// It is called on every poll cycle so that token rotation takes effect without
// restarting the controller.
// p.reader (a non-cached client) is used so that controller-runtime does not
// register a Secret informer, which would require list+watch RBAC at cluster
// scope and block WaitForCacheSync.
func (p *LinodeMaintenancePoller) readToken(ctx context.Context) (string, error) {
	secret := &corev1.Secret{}
	if err := p.reader.Get(ctx, types.NamespacedName{
		Name:      p.cfg.SecretName,
		Namespace: p.cfg.SecretNamespace,
	}, secret); err != nil {
		return "", fmt.Errorf("getting Secret %s/%s: %w",
			p.cfg.SecretNamespace, p.cfg.SecretName, err)
	}

	raw, ok := secret.Data[p.cfg.SecretKey]
	if !ok {
		return "", fmt.Errorf("key %q not found in Secret %s/%s",
			p.cfg.SecretKey, p.cfg.SecretNamespace, p.cfg.SecretName)
	}

	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("linode token in Secret %s/%s key %q is empty",
			p.cfg.SecretNamespace, p.cfg.SecretName, p.cfg.SecretKey)
	}
	return token, nil
}

// parseLinodeID extracts the numeric Linode instance ID from a providerID string
// of the form "linode://<id>".
func parseLinodeID(providerID string) (int64, error) {
	const prefix = "linode://"
	if !strings.HasPrefix(providerID, prefix) {
		return 0, fmt.Errorf("providerID %q does not begin with %q", providerID, prefix)
	}
	idStr := strings.TrimPrefix(providerID, prefix)
	if idStr == "" {
		return 0, fmt.Errorf("providerID %q has no ID after the prefix", providerID)
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing LinodeID from providerID %q: %w", providerID, err)
	}
	return id, nil
}
