/*
Copyright 2026 Rajan Sharma.

Use of this source code is governed by the MIT license
that can be found in the LICENSE file.
*/

// Package probe implements pure, client-independent health evaluation for
// SelfHealPolicy targets. Nothing in this package talks to the Kubernetes
// API; it only inspects the pod objects it is handed, which makes it cheap
// to unit test exhaustively.
package probe

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

// HealthResult is the outcome of evaluating a set of pods belonging to a
// single target Deployment.
type HealthResult struct {
	// ObservedRestarts is the total container restart count attributed to
	// the observation window: restarts whose most recent occurrence (as
	// reported by the container's last termination state, or by an active
	// CrashLoopBackOff waiting state) falls within [now-window, now].
	//
	// Kubernetes only reports a cumulative RestartCount and the timestamp
	// of the *last* restart per container, not a full restart history, so
	// this is a windowed approximation: a container's restarts only count
	// toward the result if its most recent restart happened inside the
	// window. A container that crashed many times long ago and has since
	// been stable contributes nothing.
	ObservedRestarts int32

	// CrashLoopBackOff is true if any container across the given pods is
	// currently waiting with reason CrashLoopBackOff.
	CrashLoopBackOff bool

	// Unhealthy is true if the pod set shows any restart or backoff signal
	// at all (ObservedRestarts > 0 || CrashLoopBackOff). Threshold
	// comparisons against a policy's FailureThreshold are the caller's
	// responsibility; this flag is a cheap convenience for callers that
	// only care about "is there any signal."
	Unhealthy bool
}

// EvaluateHealth inspects pods and reports restart/backoff signal within
// window, as of now. It performs no I/O and has no side effects.
func EvaluateHealth(pods []corev1.Pod, window time.Duration, now time.Time) HealthResult {
	var result HealthResult

	cutoff := now.Add(-window)

	for _, pod := range pods {
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.RestartCount <= 0 {
				continue
			}

			restartTime, known := lastRestartTime(cs)

			switch {
			case cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff":
				// An active CrashLoopBackOff is happening right now, so it
				// is unconditionally within the window.
				result.CrashLoopBackOff = true
				result.ObservedRestarts += cs.RestartCount
			case known && !restartTime.Before(cutoff) && !restartTime.After(now):
				result.ObservedRestarts += cs.RestartCount
			}
		}
	}

	result.Unhealthy = result.ObservedRestarts > 0 || result.CrashLoopBackOff
	return result
}

// lastRestartTime returns the timestamp of a container's most recent
// restart, if known.
func lastRestartTime(cs corev1.ContainerStatus) (time.Time, bool) {
	if cs.LastTerminationState.Terminated != nil {
		return cs.LastTerminationState.Terminated.FinishedAt.Time, true
	}
	return time.Time{}, false
}
