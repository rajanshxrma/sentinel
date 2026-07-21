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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	appsv1alpha1 "github.com/rajanshxrma/sentinel/api/v1alpha1"
	"github.com/rajanshxrma/sentinel/internal/probe"
)

const (
	// restartedAtAnnotation is patched onto a Deployment's pod template to
	// trigger a rolling restart, the same mechanism `kubectl rollout
	// restart` uses.
	restartedAtAnnotation = "sentinel.dev/restartedAt"

	conditionTypeReady    = "Ready"
	conditionTypeDegraded = "Degraded"
)

// SelfHealPolicyReconciler reconciles a SelfHealPolicy object.
type SelfHealPolicyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Ledger tracks recent remediation timestamps in memory, keyed per
	// target, for cheap cooldown/budget checks. See ledger.go.
	Ledger *targetLedger

	// Now, when set, overrides time.Now for tests. Left nil in production.
	Now func() time.Time
}

func (r *SelfHealPolicyReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=apps.sentinel.dev,resources=selfhealpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.sentinel.dev,resources=selfhealpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.sentinel.dev,resources=selfhealpolicies/finalizers,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile evaluates the health of every Deployment matched by a
// SelfHealPolicy's selector and, when a target breaches its failure
// threshold outside of cooldown and within its remediation budget,
// triggers a rolling restart. Targets that exhaust their budget are
// flipped to Degraded instead of being restarted forever.
func (r *SelfHealPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reconcileStart := time.Now()
	defer func() { reconcileDuration.Observe(time.Since(reconcileStart).Seconds()) }()

	log := logf.FromContext(ctx)
	now := r.now()

	var policy appsv1alpha1.SelfHealPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get SelfHealPolicy: %w", err)
	}

	applyDefaults(&policy.Spec)

	previousPhaseByTarget := make(map[string]appsv1alpha1.TargetPhase, len(policy.Status.Targets))
	previousByTarget := make(map[string]appsv1alpha1.TargetStatus, len(policy.Status.Targets))
	for _, t := range policy.Status.Targets {
		previousPhaseByTarget[t.Name] = t.Phase
		previousByTarget[t.Name] = t
	}

	selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.Selector)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("invalid selector: %w", err)
	}

	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments, client.InNamespace(policy.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return ctrl.Result{}, fmt.Errorf("list deployments: %w", err)
	}

	var (
		newTargets             []appsv1alpha1.TargetStatus
		totalRemediationsDelta int64
		reconcileErrs          []error
		anyDegraded            bool
	)

	for i := range deployments.Items {
		deploy := &deployments.Items[i]

		pods, err := r.listTargetPods(ctx, deploy)
		if err != nil {
			reconcileErrs = append(reconcileErrs, fmt.Errorf("list pods for %s: %w", deploy.Name, err))
			continue
		}

		health := probe.EvaluateHealth(pods, policy.Spec.ObservationWindow.Duration, now)

		key := ledgerKey(policy.Namespace, policy.Name, deploy.Name)
		if prev, ok := previousByTarget[deploy.Name]; ok && prev.LastRemediation != nil {
			r.Ledger.seedIfEmpty(key, prev.RemediationCount, prev.LastRemediation.Time)
		}

		remediations := r.Ledger.windowed(key, policy.Spec.MaxRestartsWindow.Duration, now)
		remediationCount := int32(len(remediations))

		var lastRemediation *metav1.Time
		if len(remediations) > 0 {
			t := metav1.NewTime(remediations[len(remediations)-1])
			lastRemediation = &t
		}

		inCooldown := lastRemediation != nil && now.Sub(lastRemediation.Time) < policy.Spec.Cooldown.Duration
		breach := health.ObservedRestarts >= policy.Spec.FailureThreshold || health.CrashLoopBackOff
		budgetExhausted := remediationCount >= policy.Spec.MaxRestarts

		var phase appsv1alpha1.TargetPhase
		switch {
		case budgetExhausted:
			phase = appsv1alpha1.TargetPhaseDegraded
		case breach && inCooldown:
			phase = appsv1alpha1.TargetPhaseCoolingDown
		case breach:
			phase = appsv1alpha1.TargetPhaseRemediating

			if policy.Spec.DryRun {
				r.Recorder.Eventf(&policy, corev1.EventTypeNormal, "WouldRemediate",
					"target %s breached failure threshold (restarts=%d, crashLoopBackOff=%t); dry-run, no patch applied",
					deploy.Name, health.ObservedRestarts, health.CrashLoopBackOff)
			} else if err := r.triggerRestart(ctx, deploy, now); err != nil {
				reconcileErrs = append(reconcileErrs, fmt.Errorf("restart %s: %w", deploy.Name, err))
				r.Recorder.Eventf(&policy, corev1.EventTypeWarning, "RemediationFailed",
					"failed to patch deployment %s: %v", deploy.Name, err)
				// Don't record the ledger entry on failure; retry next reconcile.
				phase = appsv1alpha1.TargetPhaseCoolingDown
				break
			} else {
				r.Recorder.Eventf(&policy, corev1.EventTypeNormal, "Remediated",
					"restarted deployment %s (restarts=%d, crashLoopBackOff=%t)",
					deploy.Name, health.ObservedRestarts, health.CrashLoopBackOff)
				remediationsTotal.WithLabelValues(policy.Namespace, policy.Name, deploy.Name).Inc()
			}

			r.Ledger.record(key, now)
			t := metav1.NewTime(now)
			lastRemediation = &t
			remediationCount++
			totalRemediationsDelta++
		default:
			phase = appsv1alpha1.TargetPhaseHealthy
		}

		if phase == appsv1alpha1.TargetPhaseDegraded {
			anyDegraded = true
			if previousPhaseByTarget[deploy.Name] != appsv1alpha1.TargetPhaseDegraded {
				r.Recorder.Eventf(&policy, corev1.EventTypeWarning, "RemediationBudgetExhausted",
					"target %s exhausted its remediation budget (%d/%d within %s); remediation stopped",
					deploy.Name, remediationCount, policy.Spec.MaxRestarts, policy.Spec.MaxRestartsWindow.Duration)
			}
		}

		newTargets = append(newTargets, appsv1alpha1.TargetStatus{
			Name:             deploy.Name,
			Healthy:          !health.Unhealthy,
			ObservedRestarts: health.ObservedRestarts,
			RemediationCount: remediationCount,
			LastRemediation:  lastRemediation,
			Phase:            phase,
		})
	}

	policy.Status.Targets = newTargets
	policy.Status.TotalRemediations += totalRemediationsDelta
	policy.Status.ObservedGeneration = policy.Generation

	var degradedCount int
	for _, t := range newTargets {
		if t.Phase == appsv1alpha1.TargetPhaseDegraded {
			degradedCount++
		}
	}
	targetsDegraded.WithLabelValues(policy.Namespace, policy.Name).Set(float64(degradedCount))

	readyStatus := metav1.ConditionTrue
	readyReason := "Reconciling"
	readyMessage := "controller is evaluating targets"
	if len(reconcileErrs) > 0 {
		readyStatus = metav1.ConditionFalse
		readyReason = "ReconcileErrors"
		readyMessage = fmt.Sprintf("%d error(s) during reconciliation", len(reconcileErrs))
	}
	apimeta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
		Status:             readyStatus,
		Reason:             readyReason,
		Message:            readyMessage,
		ObservedGeneration: policy.Generation,
	})

	degradedStatus := metav1.ConditionFalse
	degradedReason := "WithinBudget"
	degradedMessage := "no target has exhausted its remediation budget"
	if anyDegraded {
		degradedStatus = metav1.ConditionTrue
		degradedReason = "RemediationBudgetExhausted"
		degradedMessage = "at least one target has exhausted its remediation budget and is no longer being remediated"
	}
	apimeta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               conditionTypeDegraded,
		Status:             degradedStatus,
		Reason:             degradedReason,
		Message:            degradedMessage,
		ObservedGeneration: policy.Generation,
	})

	if err := r.Status().Update(ctx, &policy); err != nil {
		if apierrors.IsConflict(err) {
			log.V(1).Info("status update conflict, requeueing")
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}

	requeueAfter := policy.Spec.ObservationWindow.Duration
	if policy.Spec.Cooldown.Duration < requeueAfter {
		requeueAfter = policy.Spec.Cooldown.Duration
	}

	if len(reconcileErrs) > 0 {
		return ctrl.Result{RequeueAfter: requeueAfter}, reconcileErrs[0]
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// applyDefaults fills in zero-valued spec fields with their documented
// defaults. The CRD's +kubebuilder:default markers cover values submitted
// through the API server, but unit/envtest fixtures and programmatic
// callers may construct a spec directly, so the reconciler defends here
// too.
func applyDefaults(spec *appsv1alpha1.SelfHealPolicySpec) {
	if spec.FailureThreshold == 0 {
		spec.FailureThreshold = 3
	}
	if spec.ObservationWindow.Duration == 0 {
		spec.ObservationWindow = metav1.Duration{Duration: 5 * time.Minute}
	}
	if spec.Cooldown.Duration == 0 {
		spec.Cooldown = metav1.Duration{Duration: 10 * time.Minute}
	}
	if spec.MaxRestarts == 0 {
		spec.MaxRestarts = 5
	}
	if spec.MaxRestartsWindow.Duration == 0 {
		spec.MaxRestartsWindow = metav1.Duration{Duration: time.Hour}
	}
}

// triggerRestart patches deploy's pod template with a restartedAt
// annotation, the same mechanism `kubectl rollout restart` uses to force a
// rolling restart without changing any other spec field.
func (r *SelfHealPolicyReconciler) triggerRestart(ctx context.Context, deploy *appsv1.Deployment, now time.Time) error {
	original := deploy.DeepCopy()

	if deploy.Spec.Template.Annotations == nil {
		deploy.Spec.Template.Annotations = map[string]string{}
	}
	deploy.Spec.Template.Annotations[restartedAtAnnotation] = now.UTC().Format(time.RFC3339)

	return r.Patch(ctx, deploy, client.MergeFrom(original))
}

// listTargetPods returns the pods currently owned by deploy, discovered by
// walking Deployment -> ReplicaSet -> Pod controller-owner references
// rather than trusting label matches alone (a ReplicaSet left behind by a
// prior rollout could still carry matching labels).
func (r *SelfHealPolicyReconciler) listTargetPods(ctx context.Context, deploy *appsv1.Deployment) ([]corev1.Pod, error) {
	if deploy.Spec.Selector == nil {
		return nil, nil
	}
	selector, err := metav1.LabelSelectorAsSelector(deploy.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("invalid deployment selector: %w", err)
	}

	var replicaSets appsv1.ReplicaSetList
	if err := r.List(ctx, &replicaSets, client.InNamespace(deploy.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, fmt.Errorf("list replicasets: %w", err)
	}

	ownedRS := make(map[types.UID]struct{})
	for i := range replicaSets.Items {
		rs := &replicaSets.Items[i]
		if metav1.IsControlledBy(rs, deploy) {
			ownedRS[rs.UID] = struct{}{}
		}
	}
	if len(ownedRS) == 0 {
		return nil, nil
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(deploy.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}

	result := make([]corev1.Pod, 0, len(pods.Items))
	for _, pod := range pods.Items {
		owner := metav1.GetControllerOf(&pod)
		if owner == nil {
			continue
		}
		if _, ok := ownedRS[owner.UID]; ok {
			result = append(result, pod)
		}
	}
	return result, nil
}

// mapToPolicies maps a changed Deployment or Pod back to the
// SelfHealPolicies in the same namespace whose selector matches its
// labels, so the controller reacts near-instantly instead of relying
// solely on the periodic requeue.
func (r *SelfHealPolicyReconciler) mapToPolicies(ctx context.Context, obj client.Object) []reconcile.Request {
	var policies appsv1alpha1.SelfHealPolicyList
	if err := r.List(ctx, &policies, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}

	objLabels := labels.Set(obj.GetLabels())

	var requests []reconcile.Request
	for _, p := range policies.Items {
		selector, err := metav1.LabelSelectorAsSelector(&p.Spec.Selector)
		if err != nil {
			continue
		}
		if selector.Matches(objLabels) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: p.Namespace, Name: p.Name},
			})
		}
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *SelfHealPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Ledger == nil {
		r.Ledger = newTargetLedger()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.SelfHealPolicy{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.mapToPolicies)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.mapToPolicies)).
		Named("selfhealpolicy").
		Complete(r)
}
