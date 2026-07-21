/*
Copyright 2026 Rajan Sharma.

Use of this source code is governed by the MIT license
that can be found in the LICENSE file.
*/

package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// remediationsTotal counts every rolling restart the controller has
	// actually patched into a Deployment (dry-run evaluations are
	// reflected in events and status, not this counter).
	remediationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sentinel_remediations_total",
		Help: "Total number of rolling restarts triggered by SelfHealPolicy controllers.",
	}, []string{"namespace", "policy", "deployment"})

	// targetsDegraded reports the current number of targets, per policy,
	// that have exhausted their remediation budget and are no longer
	// being remediated.
	targetsDegraded = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sentinel_targets_degraded",
		Help: "Current number of targets in the Degraded phase (remediation budget exhausted), per policy.",
	}, []string{"namespace", "policy"})

	// reconcileDuration observes how long each Reconcile call takes.
	reconcileDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "sentinel_reconcile_duration_seconds",
		Help:    "Duration of SelfHealPolicy reconcile loop invocations, in seconds.",
		Buckets: prometheus.DefBuckets,
	})
)

func init() {
	metrics.Registry.MustRegister(remediationsTotal, targetsDegraded, reconcileDuration)
}
