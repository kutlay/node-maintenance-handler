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

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	maintenancev1 "linode.com/node-maintenance-controller/api/v1"
)

const (
	// labelUpcomingMaintenance is set to "true" on nodes entering maintenance.
	labelUpcomingMaintenance = "maintenance.linode.com/upcoming-maintenance"

	// labelWindowStart records the scheduled maintenance start time (RFC3339).
	labelWindowStart = "maintenance.linode.com/window-start"

	// taintKey is the taint key applied with a NoSchedule effect.
	taintKey = "maintenance.linode.com/upcoming-maintenance"

	// taintValue is the value of the NoSchedule taint.
	taintValue = "true"

	// conditionTypeUpcomingMaintenance is the Node condition type this controller manages.
	conditionTypeUpcomingMaintenance = "UpcomingMaintenance"

	// conditionReasonScheduled is the reason string for the maintenance condition.
	conditionReasonScheduled = "LinodeMaintenanceScheduled"
)

// NodeMaintenanceReconciler reconciles NodeMaintenance objects.
// It watches for changes driven by the LinodeMaintenancePoller and applies or
// removes three signals (condition, label, taint) on the associated Node.
type NodeMaintenanceReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// UncordonDelay is the duration the reconciler waits after maintenance
	// completes (i.e. after the poller sets phase=Completed) before uncordoning
	// the node. A zero value means uncordon immediately.
	UncordonDelay time.Duration
}

// +kubebuilder:rbac:groups=maintenance.linode.com,resources=nodemaintenances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=maintenance.linode.com,resources=nodemaintenances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=maintenance.linode.com,resources=nodemaintenances/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=nodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile reads the NodeMaintenance object identified by req and reconciles
// the associated Kubernetes Node to match the desired state.
func (r *NodeMaintenanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	nm := &maintenancev1.NodeMaintenance{}
	if err := r.Get(ctx, req.NamespacedName, nm); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: nm.Spec.NodeName}, node); err != nil {
		if errors.IsNotFound(err) {
			// Node is gone; ownerReference GC will delete this NodeMaintenance.
			log.Info("Node no longer exists; skipping reconcile", "node", nm.Spec.NodeName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting Node %s: %w", nm.Spec.NodeName, err)
	}

	switch nm.Status.Phase {
	case maintenancev1.MaintenancePhaseCompleted:
		return r.reconcileCompleted(ctx, nm, node)
	default:
		// Pending, Active, or empty (object just created before poller set status).
		return r.reconcileActive(ctx, nm, node)
	}
}

// reconcileActive ensures the three maintenance signals are present on the Node.
func (r *NodeMaintenanceReconciler) reconcileActive(
	ctx context.Context,
	nm *maintenancev1.NodeMaintenance,
	node *corev1.Node,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Applying maintenance signals to Node",
		"node", node.Name, "phase", nm.Status.Phase, "scheduledAt", nm.Spec.ScheduledAt)

	if err := r.syncNodeLabelsAndTaint(ctx, node.Name, nm, true); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.syncNodeCondition(ctx, node.Name, nm, true); err != nil {
		return ctrl.Result{}, err
	}

	// Update status flags so observers can confirm signals are in place.
	patch := client.MergeFrom(nm.DeepCopy())
	nm.Status.NodeLabelApplied = true
	nm.Status.NodeTaintApplied = true
	nm.Status.NodeConditionApplied = true
	if err := r.Status().Patch(ctx, nm, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating NodeMaintenance status flags: %w", err)
	}
	return ctrl.Result{}, nil
}

// reconcileCompleted removes all maintenance signals from the Node, optionally
// uncordons it, and deletes the NodeMaintenance object.
func (r *NodeMaintenanceReconciler) reconcileCompleted(
	ctx context.Context,
	nm *maintenancev1.NodeMaintenance,
	node *corev1.Node,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Honour a configurable delay before uncordoning.
	if r.UncordonDelay > 0 && nm.Status.LastSyncTime != nil {
		elapsed := time.Since(nm.Status.LastSyncTime.Time)
		if elapsed < r.UncordonDelay {
			remaining := r.UncordonDelay - elapsed
			log.Info("Waiting before post-maintenance cleanup", "node", node.Name,
				"remaining", remaining)
			return ctrl.Result{RequeueAfter: remaining}, nil
		}
	}

	log.Info("Removing maintenance signals from Node", "node", node.Name)

	if err := r.syncNodeLabelsAndTaint(ctx, node.Name, nm, false); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.syncNodeCondition(ctx, node.Name, nm, false); err != nil {
		return ctrl.Result{}, err
	}

	// Uncordon the node only if it was schedulable before maintenance started.
	if nm.Spec.WasSchedulable {
		current := &corev1.Node{}
		if err := r.Get(ctx, types.NamespacedName{Name: nm.Spec.NodeName}, current); err != nil {
			if !errors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("re-fetching Node %s for uncordon: %w", nm.Spec.NodeName, err)
			}
		} else if current.Spec.Unschedulable {
			patch := client.MergeFrom(current.DeepCopy())
			current.Spec.Unschedulable = false
			if err := r.Patch(ctx, current, patch); err != nil {
				return ctrl.Result{}, fmt.Errorf("uncordoning Node %s: %w", current.Name, err)
			}
			log.Info("Uncordoned Node after maintenance completion", "node", current.Name)
		}
	}

	// Delete the NodeMaintenance object.
	if err := r.Delete(ctx, nm); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, fmt.Errorf("deleting NodeMaintenance for node %s: %w", nm.Spec.NodeName, err)
	}
	log.Info("Deleted NodeMaintenance after maintenance completion", "node", nm.Spec.NodeName)
	return ctrl.Result{}, nil
}

// syncNodeLabelsAndTaint applies (apply=true) or removes (apply=false) the
// maintenance label and NoSchedule taint on the Node in a single PATCH call.
func (r *NodeMaintenanceReconciler) syncNodeLabelsAndTaint(
	ctx context.Context,
	nodeName string,
	nm *maintenancev1.NodeMaintenance,
	apply bool,
) error {
	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return client.IgnoreNotFound(err)
	}

	patch := client.MergeFrom(node.DeepCopy())
	changed := false

	// Kubernetes label values may only contain alphanumeric chars, '-', '_', and '.'.
	// RFC3339 contains ':' which is forbidden, so we replace ':' with '.'.
	// Example: "2026-05-27T17.24.42Z" — still human-readable and parseable.
	windowStart := strings.ReplaceAll(nm.Spec.ScheduledAt.UTC().Format(time.RFC3339), ":", ".")

	if apply {
		// --- Apply labels ---
		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}
		if node.Labels[labelUpcomingMaintenance] != "true" ||
			node.Labels[labelWindowStart] != windowStart {
			node.Labels[labelUpcomingMaintenance] = "true"
			node.Labels[labelWindowStart] = windowStart
			changed = true
		}

		// --- Apply taint ---
		hasTaint := false
		for _, t := range node.Spec.Taints {
			if t.Key == taintKey && t.Effect == corev1.TaintEffectNoSchedule {
				hasTaint = true
				break
			}
		}
		if !hasTaint {
			node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{
				Key:    taintKey,
				Value:  taintValue,
				Effect: corev1.TaintEffectNoSchedule,
			})
			changed = true
		}
	} else {
		// --- Remove labels ---
		if _, ok := node.Labels[labelUpcomingMaintenance]; ok {
			delete(node.Labels, labelUpcomingMaintenance)
			delete(node.Labels, labelWindowStart)
			changed = true
		}

		// --- Remove taint ---
		newTaints := make([]corev1.Taint, 0, len(node.Spec.Taints))
		for _, t := range node.Spec.Taints {
			if t.Key == taintKey && t.Effect == corev1.TaintEffectNoSchedule {
				changed = true
				continue
			}
			newTaints = append(newTaints, t)
		}
		if changed {
			node.Spec.Taints = newTaints
		}
	}

	if !changed {
		return nil
	}
	return r.Patch(ctx, node, patch)
}

// syncNodeCondition applies (apply=true) or removes (apply=false) the
// UpcomingMaintenance condition on the Node status.
func (r *NodeMaintenanceReconciler) syncNodeCondition(
	ctx context.Context,
	nodeName string,
	nm *maintenancev1.NodeMaintenance,
	apply bool,
) error {
	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return client.IgnoreNotFound(err)
	}

	patch := client.MergeFrom(node.DeepCopy())
	now := metav1.Now()
	changed := false

	if apply {
		message := fmt.Sprintf("Maintenance window: %s (type: %s)",
			nm.Spec.ScheduledAt.UTC().Format(time.RFC3339),
			nm.Spec.MaintenanceType,
		)
		newCond := corev1.NodeCondition{
			Type:               corev1.NodeConditionType(conditionTypeUpcomingMaintenance),
			Status:             corev1.ConditionTrue,
			Reason:             conditionReasonScheduled,
			Message:            message,
			LastTransitionTime: now,
			LastHeartbeatTime:  now,
		}

		updated := false
		for i, c := range node.Status.Conditions {
			if string(c.Type) == conditionTypeUpcomingMaintenance {
				if c.Status == corev1.ConditionTrue && c.Message == message {
					// Refresh heartbeat only.
					node.Status.Conditions[i].LastHeartbeatTime = now
				} else {
					node.Status.Conditions[i] = newCond
				}
				updated = true
				changed = true
				break
			}
		}
		if !updated {
			node.Status.Conditions = append(node.Status.Conditions, newCond)
			changed = true
		}
	} else {
		newConditions := make([]corev1.NodeCondition, 0, len(node.Status.Conditions))
		for _, c := range node.Status.Conditions {
			if string(c.Type) == conditionTypeUpcomingMaintenance {
				changed = true
				continue
			}
			newConditions = append(newConditions, c)
		}
		if changed {
			node.Status.Conditions = newConditions
		}
	}

	if !changed {
		return nil
	}
	return r.Status().Patch(ctx, node, patch)
}

// SetupWithManager registers the reconciler with the manager.
func (r *NodeMaintenanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&maintenancev1.NodeMaintenance{}).
		Named("nodemaintenance").
		Complete(r)
}
