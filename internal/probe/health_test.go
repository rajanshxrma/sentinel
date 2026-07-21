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

package probe

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func podWithRestart(restarts int32, finishedAt time.Time) corev1.Pod {
	return corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "app",
					RestartCount: restarts,
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							FinishedAt: metav1.NewTime(finishedAt),
						},
					},
				},
			},
		},
	}
}

func podCrashLooping(restarts int32) corev1.Pod {
	return corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "app",
					RestartCount: restarts,
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "CrashLoopBackOff",
						},
					},
				},
			},
		},
	}
}

func podHealthy() corev1.Pod {
	return corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "app",
					RestartCount: 0,
					State: corev1.ContainerState{
						Running: &corev1.ContainerStateRunning{},
					},
				},
			},
		},
	}
}

func TestEvaluateHealth(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	window := 5 * time.Minute

	tests := []struct {
		name             string
		pods             []corev1.Pod
		wantRestarts     int32
		wantCrashLoop    bool
		wantUnhealthy    bool
		wantExactRestart bool // if false, only assert Unhealthy/CrashLoop
	}{
		{
			name:          "empty pod list",
			pods:          nil,
			wantRestarts:  0,
			wantCrashLoop: false,
			wantUnhealthy: false,
		},
		{
			name:          "single healthy pod",
			pods:          []corev1.Pod{podHealthy()},
			wantRestarts:  0,
			wantCrashLoop: false,
			wantUnhealthy: false,
		},
		{
			name:          "multiple healthy pods",
			pods:          []corev1.Pod{podHealthy(), podHealthy(), podHealthy()},
			wantRestarts:  0,
			wantCrashLoop: false,
			wantUnhealthy: false,
		},
		{
			name: "restarts within window breach threshold-relevant count",
			pods: []corev1.Pod{
				podWithRestart(4, now.Add(-1*time.Minute)),
			},
			wantRestarts:  4,
			wantCrashLoop: false,
			wantUnhealthy: true,
		},
		{
			name: "restarts exactly at window edge are included",
			pods: []corev1.Pod{
				podWithRestart(2, now.Add(-window)),
			},
			wantRestarts:  2,
			wantCrashLoop: false,
			wantUnhealthy: true,
		},
		{
			name: "restarts outside the window are ignored",
			pods: []corev1.Pod{
				podWithRestart(10, now.Add(-2*time.Hour)),
			},
			wantRestarts:  0,
			wantCrashLoop: false,
			wantUnhealthy: false,
		},
		{
			name: "restart timestamp in the future is ignored (clock skew safety)",
			pods: []corev1.Pod{
				podWithRestart(3, now.Add(1*time.Hour)),
			},
			wantRestarts:  0,
			wantCrashLoop: false,
			wantUnhealthy: false,
		},
		{
			name: "crashloopbackoff waiting reason detected",
			pods: []corev1.Pod{
				podCrashLooping(6),
			},
			wantRestarts:  6,
			wantCrashLoop: true,
			wantUnhealthy: true,
		},
		{
			name: "restarts summed across multiple pods within window",
			pods: []corev1.Pod{
				podWithRestart(2, now.Add(-30*time.Second)),
				podWithRestart(3, now.Add(-1*time.Minute)),
				podHealthy(),
			},
			wantRestarts:  5,
			wantCrashLoop: false,
			wantUnhealthy: true,
		},
		{
			name: "mixed in-window and out-of-window restarts only count in-window",
			pods: []corev1.Pod{
				podWithRestart(2, now.Add(-30*time.Second)),
				podWithRestart(10, now.Add(-24*time.Hour)),
			},
			wantRestarts:  2,
			wantCrashLoop: false,
			wantUnhealthy: true,
		},
		{
			name: "zero restart count container never counts even with terminated state",
			pods: []corev1.Pod{
				podWithRestart(0, now.Add(-30*time.Second)),
			},
			wantRestarts:  0,
			wantCrashLoop: false,
			wantUnhealthy: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateHealth(tt.pods, window, now)
			if got.ObservedRestarts != tt.wantRestarts {
				t.Errorf("ObservedRestarts = %d, want %d", got.ObservedRestarts, tt.wantRestarts)
			}
			if got.CrashLoopBackOff != tt.wantCrashLoop {
				t.Errorf("CrashLoopBackOff = %v, want %v", got.CrashLoopBackOff, tt.wantCrashLoop)
			}
			if got.Unhealthy != tt.wantUnhealthy {
				t.Errorf("Unhealthy = %v, want %v", got.Unhealthy, tt.wantUnhealthy)
			}
		})
	}
}

func Benchmark_EvaluateHealth(b *testing.B) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	window := 5 * time.Minute

	pods := make([]corev1.Pod, 0, 50)
	for i := 0; i < 50; i++ {
		if i%5 == 0 {
			pods = append(pods, podCrashLooping(int32(i%7+1)))
		} else if i%3 == 0 {
			pods = append(pods, podWithRestart(int32(i%4+1), now.Add(-time.Duration(i)*time.Second)))
		} else {
			pods = append(pods, podHealthy())
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		EvaluateHealth(pods, window, now)
	}
}
