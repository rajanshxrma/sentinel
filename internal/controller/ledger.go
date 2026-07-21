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
	"sync"
	"time"
)

// targetLedger is an in-memory, mutex-guarded record of recent remediation
// timestamps, keyed per target ("namespace/policy/deployment"). It backs the
// cooldown and remediation-budget checks cheaply, without a status read on
// every reconcile.
//
// The ledger is process-local: a controller restart loses it. Status is the
// durable mirror (TargetStatus.RemediationCount / LastRemediation), and the
// reconciler seeds the ledger from Status the first time it sees a target
// after startup so the budget isn't silently reset by a restart. See the
// README's "Design decisions" section for the tradeoff this implies.
type targetLedger struct {
	mu           sync.Mutex
	remediations map[string][]time.Time
}

func newTargetLedger() *targetLedger {
	return &targetLedger{remediations: make(map[string][]time.Time)}
}

func ledgerKey(policyNamespace, policyName, target string) string {
	return policyNamespace + "/" + policyName + "/" + target
}

// record appends a remediation timestamp for key.
func (l *targetLedger) record(key string, ts time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.remediations[key] = append(l.remediations[key], ts)
}

// windowed returns the remediation timestamps for key that fall within
// [now-window, now], pruning older entries from the ledger as a side
// effect so it doesn't grow unbounded.
func (l *targetLedger) windowed(key string, window time.Duration, now time.Time) []time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now.Add(-window)
	existing := l.remediations[key]
	kept := make([]time.Time, 0, len(existing))
	for _, t := range existing {
		if !t.Before(cutoff) {
			kept = append(kept, t)
		}
	}
	l.remediations[key] = kept

	out := make([]time.Time, len(kept))
	copy(out, kept)
	return out
}

// seedIfEmpty seeds the ledger for key from a previously observed
// (count, lastRemediation) pair — typically read back from Status — if the
// ledger currently has no entries for key. This is a best-effort
// reconstruction: the individual historical timestamps aren't persisted,
// so all seeded entries are placed at lastRemediation. That's sufficient
// to keep enforcing the budget across a controller restart even though it
// loses the original spacing of past remediations.
func (l *targetLedger) seedIfEmpty(key string, count int32, lastRemediation time.Time) {
	if count <= 0 || lastRemediation.IsZero() {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.remediations[key]) > 0 {
		return
	}

	seeded := make([]time.Time, count)
	for i := range seeded {
		seeded[i] = lastRemediation
	}
	l.remediations[key] = seeded
}
