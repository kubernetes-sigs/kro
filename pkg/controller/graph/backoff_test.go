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

package graph

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kubernetes-sigs/kro/pkg/controller/backoff"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
)

// TestReconcileNotReadyBacksOffThenResets drives the reconciler through several
// consecutive ErrNotReady cycles and asserts the RequeueAfter delay grows
// (capped exponential), then that a clean reconcile resets the streak back to
// the base. Guards against the typo-loop metric spike: a never-resolving
// reference must not requeue at a flat 1s forever.
func TestReconcileNotReadyBacksOffThenResets(t *testing.T) {
	g := graph("g", withFinalizer)
	cl := newClient(t, g)
	exec := &fakeExecutor{applyErr: fmt.Errorf("apply %q: %w", "n", executor.ErrNotReady)}
	r := &Reconciler{
		Client:   cl,
		Compiler: &fakeCompiler{program: &compiler.Program{Nodes: map[string]*compiler.Node{"a": {}}}},
		Registry: registry.New(),
		Executor: exec,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "g"}}

	// Consecutive not-ready reconciles back off: 1s, 2s, 4s, 8s.
	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	for i, w := range want {
		res, err := r.Reconcile(context.Background(), req)
		require.NoErrorf(t, err, "attempt %d must be a soft requeue, not a hard error", i)
		assert.Equalf(t, w, res.RequeueAfter, "attempt %d", i)
	}

	// The typo is fixed: apply now succeeds. The streak resets.
	exec.applyErr = nil
	res, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter, "a clean reconcile requeues on watch/resync, not a timer")

	// A subsequent stall starts over at the base, not where it left off.
	exec.applyErr = fmt.Errorf("apply %q: %w", "n", executor.ErrNotReady)
	res, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, backoff.Base, res.RequeueAfter, "backoff must restart at base after a clean reconcile")
}
