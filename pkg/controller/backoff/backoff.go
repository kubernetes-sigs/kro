// Copyright 2025 The Kube Resource Orchestrator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package backoff provides a per-key capped exponential requeue backoff shared
// by the Graph and instance controllers' soft ErrNotReady paths.
//
// A merely-not-ready object (a node waiting on data/readiness the cluster
// hasn't surfaced yet) is requeued rather than failed. kro cannot statically
// tell a genuinely-pending field from a permanent typo, so a flat interval
// would poll a never-resolving reference forever, flooding reconcile metrics
// and the API server. Capped exponential backoff keeps the common converging
// case snappy (first retry at the base) while decaying a never-resolving
// reference to a slow poll (Max). A clean reconcile resets the streak.
package backoff

import (
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// Base is the default first-attempt delay when no base is configured.
	Base = 1 * time.Second
	// Max caps the requeue delay for a persistently not-ready object.
	Max = 5 * time.Minute
	// Factor is the per-attempt multiplier (base, 2×base, 4×base, … capped).
	Factor = 2
)

// Tracker records per-key consecutive not-ready attempts so the requeue delay
// grows with the number of consecutive soft failures. It is safe for
// concurrent use across reconcile workers. The zero/nil Tracker is usable: a
// nil receiver always returns Base and keeps no state.
type Tracker struct {
	base     time.Duration
	mu       sync.Mutex
	attempts map[client.ObjectKey]int
}

// New constructs a Tracker whose first-attempt delay is base. A non-positive
// base falls back to Base.
func New(base time.Duration) *Tracker {
	if base <= 0 {
		base = Base
	}
	return &Tracker{base: base, attempts: make(map[client.ObjectKey]int)}
}

// Next records another consecutive not-ready attempt for key and returns the
// capped exponential requeue delay to use for it. The first attempt returns
// the base; each subsequent attempt multiplies by Factor up to Max.
func (b *Tracker) Next(key client.ObjectKey) time.Duration {
	if b == nil {
		return Base
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	n := b.attempts[key]
	b.attempts[key] = n + 1

	delay := b.base
	for range n {
		delay *= Factor
		if delay >= Max {
			return Max
		}
	}
	return delay
}

// Reset clears the recorded attempts for key, so the next not-ready cycle
// starts again from the base. Called on a clean reconcile and on delete.
func (b *Tracker) Reset(key client.ObjectKey) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.attempts, key)
}
