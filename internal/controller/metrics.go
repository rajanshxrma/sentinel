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
