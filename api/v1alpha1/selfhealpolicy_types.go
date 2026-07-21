/*
Copyright 2026 Rajan Sharma.

Use of this source code is governed by the MIT license
that can be found in the LICENSE file.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SelfHealPolicySpec defines the desired state of SelfHealPolicy.
type SelfHealPolicySpec struct {
	// selector chooses which Deployments in this namespace are managed by
	// this policy.
	// +required
	Selector metav1.LabelSelector `json:"selector"`

	// failureThreshold is the number of observed container restarts within
	// observationWindow (or a detected CrashLoopBackOff) that marks a target
	// as unhealthy.
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	FailureThreshold int32 `json:"failureThreshold,omitempty"`

	// observationWindow is the sliding time window over which restarts are
	// counted toward failureThreshold.
	// +optional
	// +kubebuilder:default="5m"
	ObservationWindow metav1.Duration `json:"observationWindow,omitempty"`

	// cooldown is the minimum time that must elapse between two remediations
	// of the same target.
	// +optional
	// +kubebuilder:default="10m"
	Cooldown metav1.Duration `json:"cooldown,omitempty"`

	// maxRestarts is the maximum number of remediations allowed for a single
	// target within maxRestartsWindow before it is marked Degraded and
	// remediation stops.
	// +optional
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	MaxRestarts int32 `json:"maxRestarts,omitempty"`

	// maxRestartsWindow is the time window over which maxRestarts is
	// enforced (the remediation budget).
	// +optional
	// +kubebuilder:default="1h"
	MaxRestartsWindow metav1.Duration `json:"maxRestartsWindow,omitempty"`

	// dryRun, when true, evaluates targets and emits events/metrics as usual
	// but never patches a Deployment.
	// +optional
	// +kubebuilder:default=false
	DryRun bool `json:"dryRun,omitempty"`
}

// TargetPhase describes the remediation lifecycle phase of a single target.
// +kubebuilder:validation:Enum=Healthy;Remediating;CoolingDown;Degraded
type TargetPhase string

const (
	// TargetPhaseHealthy means the target is within its failure threshold.
	TargetPhaseHealthy TargetPhase = "Healthy"
	// TargetPhaseRemediating means a rollout restart was just triggered.
	TargetPhaseRemediating TargetPhase = "Remediating"
	// TargetPhaseCoolingDown means the target breached its threshold again
	// but is within the cooldown window since the last remediation.
	TargetPhaseCoolingDown TargetPhase = "CoolingDown"
	// TargetPhaseDegraded means the remediation budget has been exhausted;
	// the controller has stopped remediating this target.
	TargetPhaseDegraded TargetPhase = "Degraded"
)

// TargetStatus reports the observed health and remediation history of a
// single Deployment matched by a SelfHealPolicy's selector.
type TargetStatus struct {
	// name is the Deployment name.
	// +required
	Name string `json:"name"`

	// healthy reports whether the target is currently within its failure
	// threshold.
	// +optional
	Healthy bool `json:"healthy"`

	// observedRestarts is the most recently observed restart count summed
	// across the target's pods within the observation window.
	// +optional
	ObservedRestarts int32 `json:"observedRestarts"`

	// remediationCount is the number of remediations performed against this
	// target within the current maxRestartsWindow.
	// +optional
	RemediationCount int32 `json:"remediationCount"`

	// lastRemediation is the timestamp of the most recent remediation.
	// +optional
	LastRemediation *metav1.Time `json:"lastRemediation,omitempty"`

	// phase is the current remediation lifecycle phase of this target.
	// +optional
	Phase TargetPhase `json:"phase,omitempty"`
}

// SelfHealPolicyStatus defines the observed state of SelfHealPolicy.
type SelfHealPolicyStatus struct {
	// conditions represent the current state of the SelfHealPolicy resource.
	//
	// Standard condition types include:
	// - "Ready": the controller is actively evaluating and remediating targets
	// - "Degraded": at least one target has exhausted its remediation budget
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// targets reports per-Deployment health and remediation state for every
	// target currently matched by selector.
	// +optional
	Targets []TargetStatus `json:"targets,omitempty"`

	// totalRemediations is the lifetime count of remediations performed by
	// this policy across all targets.
	// +optional
	TotalRemediations int64 `json:"totalRemediations"`

	// observedGeneration is the most recent generation observed by the
	// controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Targets",type=string,JSONPath=".status.targets[*].name",description="Deployments matched by this policy"
// +kubebuilder:printcolumn:name="Remediations",type=integer,JSONPath=".status.totalRemediations",description="Lifetime remediation count"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// SelfHealPolicy is the Schema for the selfhealpolicies API.
type SelfHealPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SelfHealPolicy
	// +required
	Spec SelfHealPolicySpec `json:"spec"`

	// status defines the observed state of SelfHealPolicy
	// +optional
	Status SelfHealPolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SelfHealPolicyList contains a list of SelfHealPolicy
type SelfHealPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SelfHealPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SelfHealPolicy{}, &SelfHealPolicyList{})
		return nil
	})
}
