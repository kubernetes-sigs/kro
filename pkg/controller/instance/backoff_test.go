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

package instance

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubernetes-sigs/kro/pkg/controller/backoff"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
)

func backoffKey(ns, name string) client.ObjectKey {
	return client.ObjectKey{Namespace: ns, Name: name}
}

// notReadyErr wraps executor.ErrNotReady the way the executor apply path does.
func notReadyErr() error {
	return fmt.Errorf("apply %q: %w", "node", executor.ErrNotReady)
}

// TestNotReadyRequeueBacksOffThenResets drives the soft not-ready return site
// (notReadyRequeue) through several consecutive ErrNotReady cycles for the same
// instance and asserts the RequeueAfter delay grows (capped exponential), then
// that a reset (clean reconcile) restarts the streak at the base. Unit guard
// for the metric-flood defect: a never-resolving reference must not requeue at
// a flat interval forever.
func TestNotReadyRequeueBacksOffThenResets(t *testing.T) {
	c := &Controller{
		reconcileConfig: ReconcileConfig{DefaultRequeueDuration: 3 * time.Second},
	}
	c.ensureBackoff()
	k := backoffKey("default", "demo")

	// Consecutive not-ready reconciles back off from the configured base: 3s, 6s, 12s, 24s.
	want := []time.Duration{3 * time.Second, 6 * time.Second, 12 * time.Second, 24 * time.Second}
	for i, w := range want {
		err := c.notReadyRequeue(k, notReadyErr())
		require.Truef(t, requeue.IsRequeueError(err), "attempt %d must be a soft requeue", i)
		ra, ok := err.(*requeue.RequeueNeededAfter)
		require.Truef(t, ok, "attempt %d must be RequeueNeededAfter, got %T", i, err)
		assert.Equalf(t, w, ra.Duration(), "attempt %d", i)
	}

	// The reference resolves: a clean reconcile resets the streak.
	c.backoff.Reset(k)

	// A subsequent stall starts over at the base, not where it left off.
	err := c.notReadyRequeue(k, notReadyErr())
	ra, ok := err.(*requeue.RequeueNeededAfter)
	require.True(t, ok)
	assert.Equal(t, 3*time.Second, ra.Duration(), "backoff must restart at base after a clean reconcile")
}

// TestNotReadyRequeueCapsAtMax asserts a persistently not-ready instance decays
// to backoff.Max rather than growing without bound.
func TestNotReadyRequeueCapsAtMax(t *testing.T) {
	c := &Controller{
		reconcileConfig: ReconcileConfig{DefaultRequeueDuration: 1 * time.Second},
	}
	c.ensureBackoff()
	k := backoffKey("default", "capped")

	var last time.Duration
	for range 40 {
		err := c.notReadyRequeue(k, notReadyErr())
		ra, ok := err.(*requeue.RequeueNeededAfter)
		require.True(t, ok)
		last = ra.Duration()
	}
	assert.Equal(t, backoff.Max, last, "delay must saturate at backoff.Max")
}

// TestNotReadyRequeueDisabledHonorsNone asserts that when the operator disabled
// delayed requeues (DefaultRequeueDuration==0), the soft not-ready path emits
// requeue.None and does not force a timer.
func TestNotReadyRequeueDisabledHonorsNone(t *testing.T) {
	c := &Controller{
		reconcileConfig: ReconcileConfig{DefaultRequeueDuration: 0},
	}
	c.ensureBackoff()
	k := backoffKey("default", "disabled")

	err := c.notReadyRequeue(k, notReadyErr())
	_, isAfter := err.(*requeue.RequeueNeededAfter)
	assert.False(t, isAfter, "disabled requeues must not force a timed requeue")
	_, isNone := err.(*requeue.NoRequeue)
	assert.True(t, isNone, "disabled requeues must emit requeue.None")
}
