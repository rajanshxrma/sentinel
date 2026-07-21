//go:build e2e
// +build e2e

/*
Copyright 2026 Rajan Sharma.

Use of this source code is governed by the MIT license
that can be found in the LICENSE file.
*/

// Package e2e exercises the real SelfHealPolicy control loop end to end
// against an actual kubelet: a kind cluster, the built manager image, and
// the hand-authored Helm chart. It is excluded from `make test` (which
// stays Docker-free) via the e2e build tag and is instead run with
// `make test-e2e` or `go test -tags e2e ./test/e2e/...`.
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rajanshxrma/sentinel/test/utils"
)

const (
	kindClusterName = "sentinel-e2e"
	e2eNamespace    = "sentinel-system"
	e2eImage        = "sentinel:e2e"
	helmRelease     = "sentinel"
	chartPath       = "../../charts/sentinel"
)

// keepCluster, when true (E2E_KEEP_CLUSTER=true), skips deleting the kind
// cluster in AfterSuite — useful when iterating on the suite locally.
var keepCluster = os.Getenv("E2E_KEEP_CLUSTER") == "true"

// createdCluster tracks whether this suite created the kind cluster (and
// therefore owns tearing it down) versus reusing one that already existed.
var createdCluster bool

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting sentinel kind e2e suite\n")
	RunSpecs(t, "sentinel e2e suite")
}

var _ = BeforeSuite(func() {
	By("ensuring a kind cluster exists")
	out, _ := utils.Run(exec.Command("kind", "get", "clusters"))
	if !containsLine(out, kindClusterName) {
		By("creating the kind cluster")
		_, err := utils.Run(exec.Command("kind", "create", "cluster", "--name", kindClusterName))
		Expect(err).NotTo(HaveOccurred(), "failed to create kind cluster")
		createdCluster = true
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "reusing existing kind cluster %q\n", kindClusterName)
	}

	By("building the manager image")
	buildCmd := exec.Command("docker", "build", "--platform", "linux/arm64", "-t", e2eImage, ".")
	_, err := utils.Run(buildCmd)
	Expect(err).NotTo(HaveOccurred(), "failed to build manager image")

	By("loading the manager image into kind")
	loadCmd := exec.Command("kind", "load", "docker-image", e2eImage, "--name", kindClusterName)
	_, err = utils.Run(loadCmd)
	Expect(err).NotTo(HaveOccurred(), "failed to load image into kind")

	By("installing the sentinel helm chart")
	helmCmd := exec.Command("helm", "upgrade", "--install", helmRelease, chartPath,
		"--namespace", e2eNamespace,
		"--create-namespace",
		"--set", "image.repository=sentinel",
		"--set", "image.tag=e2e",
		"--set", "image.pullPolicy=Never",
		"--set", "metrics.secure=false",
		"--wait", "--timeout", "120s",
	)
	_, err = utils.Run(helmCmd)
	Expect(err).NotTo(HaveOccurred(), "failed to helm install sentinel")
})

var _ = AfterSuite(func() {
	By("uninstalling the sentinel helm release")
	_, _ = utils.Run(exec.Command("helm", "uninstall", helmRelease, "--namespace", e2eNamespace))

	By("deleting the sentinel-system namespace")
	_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", e2eNamespace, "--ignore-not-found", "--timeout=60s"))

	if createdCluster && !keepCluster {
		By("deleting the kind cluster")
		_, _ = utils.Run(exec.Command("kind", "delete", "cluster", "--name", kindClusterName))
	} else if keepCluster {
		_, _ = fmt.Fprintf(GinkgoWriter, "E2E_KEEP_CLUSTER=true: leaving kind cluster %q running\n", kindClusterName)
	}
})

func containsLine(output, target string) bool {
	for _, line := range utils.GetNonEmptyLines(output) {
		if line == target {
			return true
		}
	}
	return false
}
