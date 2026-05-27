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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeMaintenancePhase represents the lifecycle phase of a NodeMaintenance.
// +kubebuilder:validation:Enum=Pending;Active;Completed
type NodeMaintenancePhase string

const (
	// MaintenancePhasePending means the maintenance window has not started yet;
	// signals have been applied to the Node and we are waiting for the window.
	MaintenancePhasePending NodeMaintenancePhase = "Pending"

	// MaintenancePhaseActive means the maintenance window start time has passed
	// and the Linode instance is still listed in the Linode maintenance API.
	MaintenancePhaseActive NodeMaintenancePhase = "Active"

	// MaintenancePhaseCompleted means the Linode API no longer lists this
	// maintenance. Signals are being removed and this object will be deleted.
	MaintenancePhaseCompleted NodeMaintenancePhase = "Completed"
)

// LinodeEntity mirrors the entity block from the Linode maintenance API response.
type LinodeEntity struct {
	// ID is the entity's numeric ID (same as LinodeID for Linode instances).
	ID int64 `json:"id"`

	// Label is the human-readable label of the Linode instance.
	Label string `json:"label"`

	// Type is the entity type, e.g. "linode".
	Type string `json:"type"`

	// URL is the API URL for this entity.
	// +optional
	URL string `json:"url,omitempty"`
}

// NodeMaintenanceSpec defines the desired state of NodeMaintenance.
type NodeMaintenanceSpec struct {
	// NodeName is the name of the Kubernetes Node this maintenance is associated with.
	// Immutable after creation.
	// +kubebuilder:validation:Required
	NodeName string `json:"nodeName"`

	// LinodeID is the numeric Linode instance ID extracted from the Node's
	// spec.providerID field (format: "linode://<id>"). Immutable after creation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	LinodeID int64 `json:"linodeID"`

	// ScheduledAt is the time the Linode maintenance window begins,
	// as reported by the Linode maintenance API. Immutable after creation.
	// +kubebuilder:validation:Required
	ScheduledAt metav1.Time `json:"scheduledAt"`

	// MaintenanceType is the type of maintenance as returned by the Linode API
	// (e.g. "reboot"). Informational only.
	// +optional
	MaintenanceType string `json:"maintenanceType,omitempty"`

	// Entity describes the Linode entity subject to maintenance.
	// +optional
	Entity *LinodeEntity `json:"entity,omitempty"`

	// WasSchedulable records whether the Node was schedulable
	// (spec.unschedulable == false) at the time this NodeMaintenance was created.
	// Used to decide whether to uncordon the node after maintenance ends.
	// Immutable after creation.
	// +kubebuilder:validation:Required
	WasSchedulable bool `json:"wasSchedulable"`

	// MaintenanceWindow is the look-ahead duration. If the scheduled maintenance
	// falls within this duration from now, signals are applied to the Node.
	// Defaults to 24h if not set.
	// +optional
	// +kubebuilder:default="24h"
	MaintenanceWindow *metav1.Duration `json:"maintenanceWindow,omitempty"`
}

// NodeMaintenanceStatus defines the observed state of NodeMaintenance.
type NodeMaintenanceStatus struct {
	// Phase is the current lifecycle phase of this maintenance.
	// +optional
	Phase NodeMaintenancePhase `json:"phase,omitempty"`

	// NodeConditionApplied indicates the UpcomingMaintenance node condition
	// has been successfully written to the Node.
	// +optional
	NodeConditionApplied bool `json:"nodeConditionApplied,omitempty"`

	// NodeLabelApplied indicates the maintenance labels have been applied to the Node.
	// +optional
	NodeLabelApplied bool `json:"nodeLabelApplied,omitempty"`

	// NodeTaintApplied indicates the NoSchedule taint has been applied to the Node.
	// +optional
	NodeTaintApplied bool `json:"nodeTaintApplied,omitempty"`

	// LastSyncTime is the last time the poller successfully synced this object
	// against the Linode API. The reconciler uses this as the reference time for
	// post-maintenance uncordon delays.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// Conditions contains standard Kubernetes condition fields for
	// machine-readable status (e.g. for NHC/SNR integration).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Node",type="string",JSONPath=".spec.nodeName"
// +kubebuilder:printcolumn:name="LinodeID",type="integer",JSONPath=".spec.linodeID"
// +kubebuilder:printcolumn:name="Window",type="string",JSONPath=".spec.scheduledAt"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// NodeMaintenance is the Schema for the nodemaintenances API.
// One NodeMaintenance object exists per Kubernetes Node that has an upcoming
// Linode maintenance event within the configured look-ahead window.
// The object is cluster-scoped (matching the scope of the Node resource) so
// that Kubernetes garbage collection via ownerReferences works correctly.
type NodeMaintenance struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of NodeMaintenance.
	// +optional
	Spec NodeMaintenanceSpec `json:"spec,omitempty"`

	// status defines the observed state of NodeMaintenance.
	// +optional
	Status NodeMaintenanceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NodeMaintenanceList contains a list of NodeMaintenance.
type NodeMaintenanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeMaintenance `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NodeMaintenance{}, &NodeMaintenanceList{})
}
