//go:build e2e
// +build e2e

/*
Copyright 2026 Rajan Sharma.

Use of this source code is governed by the MIT license
that can be found in the LICENSE file.
*/

package e2e

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rajanshxrma/sentinel/test/utils"
)

// crashingDeploymentManifest is a Deployment whose only container never
// writes /tmp/healthy, so its liveness probe fails immediately and the
// kubelet restarts it in a real CrashLoopBackOff — exercising the operator
// against an actual kubelet, not a fake.
func crashingDeploymentManifest(name string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    app: %[1]s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: %[1]s
  template:
    metadata:
      labels:
        app: %[1]s
    spec:
      containers:
        - name: app
          image: busybox
          command: ["sh", "-c", "sleep 3600"]
          livenessProbe:
            exec:
              command: ["cat", "/tmp/healthy"]
            initialDelaySeconds: 1
            periodSeconds: 2
            failureThreshold: 1
`, name, e2eNamespace)
}

func selfHealPolicyManifest(name, targetName string) string {
	return fmt.Sprintf(`apiVersion: apps.sentinel.dev/v1alpha1
kind: SelfHealPolicy
metadata:
  name: %[1]s
  namespace: %[3]s
spec:
  selector:
    matchLabels:
      app: %[2]s
  failureThreshold: 2
  observationWindow: 2m
  cooldown: 10s
  maxRestarts: 2
  maxRestartsWindow: 5m
`, name, targetName, e2eNamespace)
}

func kubectlApplyStdin(manifest string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	return err
}

func kubectlDelete(kind, name string) {
	_, _ = utils.Run(exec.Command("kubectl", "delete", kind, name, "-n", e2eNamespace, "--ignore-not-found", "--timeout=60s"))
}

func kubectlJSONPath(kind, name, jsonpath string) (string, error) {
	out, err := utils.Run(exec.Command("kubectl", "get", kind, name, "-n", e2eNamespace,
		"-o", fmt.Sprintf("jsonpath=%s", jsonpath)))
	return strings.TrimSpace(out), err
}

func rolloutRevisionCount(name string) (int, error) {
	out, err := utils.Run(exec.Command("kubectl", "rollout", "history", "deployment/"+name, "-n", e2eNamespace))
	if err != nil {
		return 0, err
	}
	count := 0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if _, err := strconv.Atoi(fields[0]); err == nil {
			count++
		}
	}
	return count, nil
}

var _ = Describe("SelfHealPolicy", Ordered, func() {
	const (
		targetName = "crashy-app"
		policyName = "crashy-app-policy"
	)

	BeforeAll(func() {
		By("deploying a deployment whose liveness probe always fails")
		Expect(kubectlApplyStdin(crashingDeploymentManifest(targetName))).To(Succeed())

		By("creating a SelfHealPolicy that watches it")
		Expect(kubectlApplyStdin(selfHealPolicyManifest(policyName, targetName))).To(Succeed())
	})

	AfterAll(func() {
		kubectlDelete("selfhealpolicy", policyName)
		kubectlDelete("deployment", targetName)
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			logs, _ := utils.Run(exec.Command("kubectl", "logs", "-n", e2eNamespace,
				"-l", "control-plane=controller-manager", "--tail=200"))
			_, _ = fmt.Fprintf(GinkgoWriter, "controller logs:\n%s\n", logs)

			events, _ := utils.Run(exec.Command("kubectl", "get", "events", "-n", e2eNamespace,
				"--sort-by=.lastTimestamp"))
			_, _ = fmt.Fprintf(GinkgoWriter, "events:\n%s\n", events)

			policy, _ := utils.Run(exec.Command("kubectl", "get", "selfhealpolicy", policyName,
				"-n", e2eNamespace, "-o", "yaml"))
			_, _ = fmt.Fprintf(GinkgoWriter, "policy:\n%s\n", policy)
		}
	})

	It("heals the crash-looping deployment and then stops at the remediation budget", func() {
		By("waiting for the operator to patch the restartedAt annotation")
		Eventually(func(g Gomega) {
			v, err := kubectlJSONPath("deployment", targetName,
				"{.spec.template.metadata.annotations.sentinel\\.dev/restartedAt}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(v).NotTo(BeEmpty())
		}, 90*time.Second, 2*time.Second).Should(Succeed())

		By("confirming kubectl rollout history shows more than one revision")
		Eventually(func(g Gomega) {
			n, err := rolloutRevisionCount(targetName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(n).To(BeNumerically(">=", 2))
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		By("confirming a Remediated event fired")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "events", "-n", e2eNamespace,
				"--field-selector", "reason=Remediated", "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(utils.GetNonEmptyLines(out)).NotTo(BeEmpty())
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		By("confirming status.totalRemediations is greater than zero")
		Eventually(func(g Gomega) {
			v, err := kubectlJSONPath("selfhealpolicy", policyName, "{.status.totalRemediations}")
			g.Expect(err).NotTo(HaveOccurred())
			n, convErr := strconv.Atoi(v)
			g.Expect(convErr).NotTo(HaveOccurred())
			g.Expect(n).To(BeNumerically(">", 0))
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		By("waiting for the target to exhaust its remediation budget and flip Degraded")
		Eventually(func(g Gomega) {
			phase, err := kubectlJSONPath("selfhealpolicy", policyName, "{.status.targets[0].phase}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(phase).To(Equal("Degraded"))

			degraded, err := kubectlJSONPath("selfhealpolicy", policyName,
				`{.status.conditions[?(@.type=="Degraded")].status}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(degraded).To(Equal("True"))
		}, 120*time.Second, 3*time.Second).Should(Succeed())

		By("recording remediation state once degraded")
		revisionsAtDegraded, err := rolloutRevisionCount(targetName)
		Expect(err).NotTo(HaveOccurred())
		totalAtDegraded, err := kubectlJSONPath("selfhealpolicy", policyName, "{.status.totalRemediations}")
		Expect(err).NotTo(HaveOccurred())

		By("confirming no further remediations occur once degraded")
		Consistently(func(g Gomega) {
			n, err := rolloutRevisionCount(targetName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(n).To(Equal(revisionsAtDegraded), "expected no new rollout revisions after budget exhaustion")

			total, err := kubectlJSONPath("selfhealpolicy", policyName, "{.status.totalRemediations}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(total).To(Equal(totalAtDegraded), "expected totalRemediations to stop increasing once degraded")

			phase, err := kubectlJSONPath("selfhealpolicy", policyName, "{.status.targets[0].phase}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(phase).To(Equal("Degraded"))
		}, 20*time.Second, 3*time.Second).Should(Succeed())
	})
})
