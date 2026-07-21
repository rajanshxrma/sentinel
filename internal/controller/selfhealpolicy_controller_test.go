/*
Copyright 2026 Rajan Sharma.

Use of this source code is governed by the MIT license
that can be found in the LICENSE file.
*/

package controller

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	appsv1alpha1 "github.com/rajanshxrma/sentinel/api/v1alpha1"
)

const (
	testNamespace  = "default"
	labelKeyApp    = "app"
	containerName  = "app"
	containerImage = "busybox"
)

var fixtureCounter atomic.Int64

// uniqueName returns a per-test-run unique name so parallel-safe It blocks
// never collide on Deployment/ReplicaSet/Pod/SelfHealPolicy names.
func uniqueName(prefix string) string {
	n := fixtureCounter.Add(1)
	return fmt.Sprintf("%s-%d", prefix, n)
}

func int32Ptr(i int32) *int32 { return &i }

// crashLoopingTarget creates a Deployment, a ReplicaSet controller-owned by
// it, and a Pod controller-owned by that ReplicaSet whose sole container is
// reported as CrashLoopBackOff with restartCount. There is no real
// Deployment/ReplicaSet controller running in envtest, so the ownership
// chain the reconciler walks is built by hand here.
func crashLoopingTarget(ctx context.Context, name string, restartCount int32) *appsv1.Deployment {
	labels := map[string]string{labelKeyApp: name}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: containerName, Image: containerImage}}},
			},
		},
	}
	Expect(k8sClient.Create(ctx, deploy)).To(Succeed())

	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name + "-rs",
			Namespace:       testNamespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(deploy, appsv1.SchemeGroupVersion.WithKind("Deployment"))},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: containerName, Image: containerImage}}},
			},
		},
	}
	Expect(k8sClient.Create(ctx, rs)).To(Succeed())

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name + "-pod",
			Namespace:       testNamespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(rs, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: containerName, Image: containerImage}}},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())

	pod.Status = corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{
			{
				Name:         containerName,
				RestartCount: restartCount,
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
				},
			},
		},
	}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

	return deploy
}

func deploymentAnnotation(ctx context.Context, name string) string {
	var d appsv1.Deployment
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, &d)).To(Succeed())
	return d.Spec.Template.Annotations[restartedAtAnnotation]
}

var _ = Describe("SelfHealPolicy Controller", func() {
	ctx := context.Background()

	Context("when a target breaches its failure threshold", func() {
		It("patches restartedAt, increments TotalRemediations, and respects cooldown", func() {
			name := uniqueName("remediate")
			labels := map[string]string{labelKeyApp: name}

			deploy := crashLoopingTarget(ctx, name, 5)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, deploy) })

			policy := &appsv1alpha1.SelfHealPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
				Spec: appsv1alpha1.SelfHealPolicySpec{
					Selector:          metav1.LabelSelector{MatchLabels: labels},
					FailureThreshold:  2,
					ObservationWindow: metav1.Duration{Duration: time.Minute},
					Cooldown:          metav1.Duration{Duration: 3 * time.Second},
					MaxRestarts:       5,
					MaxRestartsWindow: metav1.Duration{Duration: time.Minute},
				},
			}
			Expect(k8sClient.Create(ctx, policy)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, policy) })

			By("patching the deployment's restartedAt annotation")
			Eventually(func(g Gomega) {
				g.Expect(deploymentAnnotation(ctx, name)).NotTo(BeEmpty())
			}, 10*time.Second, 200*time.Millisecond).Should(Succeed())

			By("incrementing status.totalRemediations")
			Eventually(func(g Gomega) {
				var p appsv1alpha1.SelfHealPolicy
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, &p)).To(Succeed())
				g.Expect(p.Status.TotalRemediations).To(BeNumerically(">=", 1))
			}, 10*time.Second, 200*time.Millisecond).Should(Succeed())

			firstValue := deploymentAnnotation(ctx, name)
			Expect(firstValue).NotTo(BeEmpty())

			By("not patching again inside the cooldown window")
			Consistently(func(g Gomega) {
				g.Expect(deploymentAnnotation(ctx, name)).To(Equal(firstValue))
			}, 2*time.Second, 250*time.Millisecond).Should(Succeed())

			By("patching again once the cooldown has elapsed")
			Eventually(func(g Gomega) {
				g.Expect(deploymentAnnotation(ctx, name)).NotTo(Equal(firstValue))
			}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
		})
	})

	Context("when a target exhausts its remediation budget", func() {
		It("flips to Degraded and stops remediating", func() {
			name := uniqueName("degrade")
			labels := map[string]string{labelKeyApp: name}

			deploy := crashLoopingTarget(ctx, name, 5)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, deploy) })

			policy := &appsv1alpha1.SelfHealPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
				Spec: appsv1alpha1.SelfHealPolicySpec{
					Selector:          metav1.LabelSelector{MatchLabels: labels},
					FailureThreshold:  2,
					ObservationWindow: metav1.Duration{Duration: time.Minute},
					Cooldown:          metav1.Duration{Duration: 1 * time.Second},
					MaxRestarts:       2,
					MaxRestartsWindow: metav1.Duration{Duration: time.Minute},
				},
			}
			Expect(k8sClient.Create(ctx, policy)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, policy) })

			By("eventually marking the target Degraded once the budget is exhausted")
			Eventually(func(g Gomega) {
				var p appsv1alpha1.SelfHealPolicy
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, &p)).To(Succeed())
				g.Expect(p.Status.Targets).NotTo(BeEmpty())
				g.Expect(p.Status.Targets[0].Phase).To(Equal(appsv1alpha1.TargetPhaseDegraded))
				g.Expect(apimeta.IsStatusConditionTrue(p.Status.Conditions, conditionTypeDegraded)).To(BeTrue())
			}, 20*time.Second, 250*time.Millisecond).Should(Succeed())

			finalValue := deploymentAnnotation(ctx, name)
			var totalAtDegraded int64
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, &appsv1alpha1.SelfHealPolicy{})).To(Succeed())
			{
				var p appsv1alpha1.SelfHealPolicy
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, &p)).To(Succeed())
				totalAtDegraded = p.Status.TotalRemediations
			}
			Expect(totalAtDegraded).To(BeNumerically(">=", 2))

			By("not patching or remediating any further once degraded")
			Consistently(func(g Gomega) {
				g.Expect(deploymentAnnotation(ctx, name)).To(Equal(finalValue))
				var p appsv1alpha1.SelfHealPolicy
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, &p)).To(Succeed())
				g.Expect(p.Status.TotalRemediations).To(Equal(totalAtDegraded))
				g.Expect(p.Status.Targets[0].Phase).To(Equal(appsv1alpha1.TargetPhaseDegraded))
			}, 4*time.Second, 250*time.Millisecond).Should(Succeed())
		})
	})

	Context("when dryRun is enabled", func() {
		It("evaluates the target but never patches the deployment", func() {
			name := uniqueName("dryrun")
			labels := map[string]string{labelKeyApp: name}

			deploy := crashLoopingTarget(ctx, name, 5)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, deploy) })

			policy := &appsv1alpha1.SelfHealPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
				Spec: appsv1alpha1.SelfHealPolicySpec{
					Selector:          metav1.LabelSelector{MatchLabels: labels},
					FailureThreshold:  2,
					ObservationWindow: metav1.Duration{Duration: time.Minute},
					Cooldown:          metav1.Duration{Duration: 1 * time.Second},
					MaxRestarts:       5,
					MaxRestartsWindow: metav1.Duration{Duration: time.Minute},
					DryRun:            true,
				},
			}
			Expect(k8sClient.Create(ctx, policy)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, policy) })

			By("still reporting the target as unhealthy in status")
			Eventually(func(g Gomega) {
				var p appsv1alpha1.SelfHealPolicy
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, &p)).To(Succeed())
				g.Expect(p.Status.Targets).NotTo(BeEmpty())
				g.Expect(p.Status.Targets[0].Healthy).To(BeFalse())
			}, 10*time.Second, 200*time.Millisecond).Should(Succeed())

			By("never patching the deployment")
			Consistently(func(g Gomega) {
				g.Expect(deploymentAnnotation(ctx, name)).To(BeEmpty())
			}, 3*time.Second, 250*time.Millisecond).Should(Succeed())
		})
	})
})
