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

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/events"
	"k8s.io/kubectl/pkg/drain"
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

	// Event reasons
	eventReasonDrainFailed    = "DrainFailed"
	eventReasonDrainSucceeded = "DrainSucceeded"
	eventReasonDrainExhausted = "DrainExhausted"

	// Event actions
	eventActionDrain = "DrainNode"
)

// NodeMaintenanceReconciler reconciles NodeMaintenance objects.
// It watches for changes driven by the LinodeMaintenancePoller and applies or
// removes three signals (condition, label, taint) on the associated Node.
// Optionally it also cordons and drains the node.
type NodeMaintenanceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// KubeClientset is the typed Kubernetes client used by the drain helper.
	// Must be set when DrainNodes is true.
	KubeClientset kubernetes.Interface

	// UncordonDelay is the duration the reconciler waits after maintenance
	// completes (i.e. after the poller sets phase=Completed) before uncordoning
	// the node. A zero value means uncordon immediately.
	UncordonDelay time.Duration

	// CordonNodes controls whether the Node is cordoned (spec.unschedulable=true)
	// when maintenance signals are applied. Independent of DrainNodes.
	CordonNodes bool

	// DrainNodes controls whether the Node is drained after being cordoned.
	// Enabling this always implies cordoning regardless of CordonNodes.
	DrainNodes bool

	// DrainTimeout is the per-attempt timeout passed to the kubectl drain helper.
	DrainTimeout time.Duration

	// DrainMaxRetries is the maximum number of drain attempts before giving up.
	// 0 means unlimited retries.
	DrainMaxRetries int

	// DrainIgnoreDaemonSets controls whether DaemonSet-owned pods are skipped
	// during drain. Should almost always be true.
	DrainIgnoreDaemonSets bool

	// DrainDeleteEmptyDirData controls whether pods with emptyDir volumes are
	// evicted during drain. Defaults to false to avoid data loss.
	DrainDeleteEmptyDirData bool

	// DrainRetryInterval is the duration to wait between failed drain attempts.
	DrainRetryInterval time.Duration
}

// +kubebuilder:rbac:groups=maintenance.linode.com,resources=nodemaintenances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=maintenance.linode.com,resources=nodemaintenances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=maintenance.linode.com,resources=nodemaintenances/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=nodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch

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

// reconcileActive ensures the three maintenance signals are present on the Node,
// optionally cordons the node, and optionally drains it with retry logic.
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

	// Cordon the node if requested by either flag.
	// DrainNodes implies cordon because draining an uncordoned node would allow
	// new pods to be scheduled onto it between evictions.
	if r.CordonNodes || r.DrainNodes {
		if err := r.cordonNode(ctx, node.Name); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Capture the baseline for the status patch (includes all further mutations).
	statusPatch := client.MergeFrom(nm.DeepCopy())
	nm.Status.NodeLabelApplied = true
	nm.Status.NodeTaintApplied = true
	nm.Status.NodeConditionApplied = true

	if !r.DrainNodes {
		if err := r.Status().Patch(ctx, nm, statusPatch); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating NodeMaintenance status: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// ---- Drain logic ----

	// Nothing left to do if drain already succeeded.
	if nm.Status.Drain != nil && nm.Status.Drain.Succeeded {
		if err := r.Status().Patch(ctx, nm, statusPatch); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating NodeMaintenance status: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Stop retrying once the configured limit is reached.
	if r.DrainMaxRetries > 0 && nm.Status.Drain != nil && nm.Status.Drain.Attempts >= r.DrainMaxRetries {
		log.Info("Drain max retries reached; node remains cordoned",
			"node", node.Name, "attempts", nm.Status.Drain.Attempts, "maxRetries", r.DrainMaxRetries)
		r.Recorder.Eventf(nm, node, corev1.EventTypeWarning, eventReasonDrainExhausted, eventActionDrain,
			"Max drain retries (%d) exhausted for node %s: %s",
			r.DrainMaxRetries, node.Name, nm.Status.Drain.LastError)
		if err := r.Status().Patch(ctx, nm, statusPatch); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating NodeMaintenance status: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Initialise drain status on first attempt.
	now := metav1.Now()
	if nm.Status.Drain == nil {
		nm.Status.Drain = &maintenancev1.DrainStatus{
			StartedAt: &now,
		}
	}
	nm.Status.Drain.Attempts++
	nm.Status.Drain.LastAttemptAt = &now

	log.Info("Draining Node", "node", node.Name, "attempt", nm.Status.Drain.Attempts,
		"timeout", r.DrainTimeout, "maxRetries", r.DrainMaxRetries)

	drainErr := r.drainNode(ctx, node.Name)

	if drainErr != nil {
		nm.Status.Drain.LastError = drainErr.Error()
		log.Info("Drain attempt failed; will retry",
			"node", node.Name, "attempt", nm.Status.Drain.Attempts,
			"error", drainErr, "retryIn", r.DrainRetryInterval)
		r.Recorder.Eventf(nm, node, corev1.EventTypeWarning, eventReasonDrainFailed, eventActionDrain,
			"Drain attempt %d for node %s failed: %s (retry in %s)",
			nm.Status.Drain.Attempts, node.Name, drainErr.Error(), r.DrainRetryInterval)
		if err := r.Status().Patch(ctx, nm, statusPatch); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating NodeMaintenance drain status: %w", err)
		}
		return ctrl.Result{RequeueAfter: r.DrainRetryInterval}, nil
	}

	nm.Status.Drain.Succeeded = true
	nm.Status.Drain.LastError = ""
	log.Info("Node drain succeeded", "node", node.Name, "attempts", nm.Status.Drain.Attempts)
	r.Recorder.Eventf(nm, node, corev1.EventTypeNormal, eventReasonDrainSucceeded, eventActionDrain,
		"Node %s drained successfully after %d attempt(s)", node.Name, nm.Status.Drain.Attempts)

	if err := r.Status().Patch(ctx, nm, statusPatch); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating NodeMaintenance drain status: %w", err)
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
	// This preserves the pre-maintenance state for nodes that were already
	// cordoned by an external actor before our controller observed them.
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

// cordonNode marks the Node as unschedulable (spec.unschedulable=true) if it
// is not already cordoned.
func (r *NodeMaintenanceReconciler) cordonNode(ctx context.Context, nodeName string) error {
	log := logf.FromContext(ctx)

	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return client.IgnoreNotFound(err)
	}
	if node.Spec.Unschedulable {
		return nil // already cordoned; nothing to do
	}
	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.Unschedulable = true
	if err := r.Patch(ctx, node, patch); err != nil {
		return fmt.Errorf("cordoning Node %s: %w", nodeName, err)
	}
	log.Info("Cordoned Node", "node", nodeName)
	return nil
}

// drainNode evicts all evictable pods from the node using the kubectl drain
// helper. The call blocks for up to DrainTimeout waiting for pods to terminate.
func (r *NodeMaintenanceReconciler) drainNode(ctx context.Context, nodeName string) error {
	log := logf.FromContext(ctx)
	helper := &drain.Helper{
		Ctx:                 ctx,
		Client:              r.KubeClientset,
		Force:               false,
		GracePeriodSeconds:  -1, // honour each pod's own terminationGracePeriodSeconds
		IgnoreAllDaemonSets: r.DrainIgnoreDaemonSets,
		DeleteEmptyDirData:  r.DrainDeleteEmptyDirData,
		Timeout:             r.DrainTimeout,
		Out:                 &drainLogger{log: log, isErr: false},
		ErrOut:              &drainLogger{log: log, isErr: true},
	}
	return drain.RunNodeDrain(helper, nodeName)
}

// drainLogger is an io.Writer that routes kubectl drain output to the
// controller's structured logger so all drain progress is visible via
// the manager's log stream.
type drainLogger struct {
	log   logr.Logger
	isErr bool
}

func (l *drainLogger) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	if msg == "" {
		return len(p), nil
	}
	if l.isErr {
		l.log.Error(nil, "[drain] "+msg)
	} else {
		l.log.Info("[drain] " + msg)
	}
	return len(p), nil
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
