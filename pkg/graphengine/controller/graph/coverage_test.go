// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package graph

// Targeted tests for the gaps audit identified after the first pass of
// correctness fixes: the new generation-skip branch in updateStatus
// and the DataPending vs WaitingForReadiness condition split.

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
	krotruntime "github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
)

// TestUpdateStatusGenerationGuard pins the skip branch of updateStatus:
// when the live object's generation has advanced past the in-memory
// copy this reconcile started with, the marker output is stale and we
// must NOT publish it. The next reconcile will catch up.
func TestUpdateStatusGenerationGuard(t *testing.T) {
	tests := []struct {
		name           string
		liveGeneration int64
		passGeneration int64
		// wantConditionCount is the count we expect on the LIVE object
		// after updateStatus returns. Stale skip → 0 (nothing
		// applied); match → 3 (Accepted + ResourcesConverged
		// dependents and the rolled-up Ready root).
		wantConditionCount int
	}{
		{
			name:               "matching-generation-flushes",
			liveGeneration:     1,
			passGeneration:     1,
			wantConditionCount: 3,
		},
		{
			name:               "stale-generation-skips",
			liveGeneration:     2,
			passGeneration:     1,
			wantConditionCount: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			live := graph("g", withFinalizer)
			live.Generation = tc.liveGeneration
			cl := newClient(t, live)
			r := &Reconciler{Client: cl, Compiler: &fakeCompiler{}, Registry: registry.New(), Executor: &fakeExecutor{}}

			// Build the in-memory copy this "reconcile pass" worked
			// against. Mutate conditions so we can tell whether the
			// patch landed.
			work := graph("g", withFinalizer)
			work.Generation = tc.passGeneration
			marker := NewConditionsMarkerFor(work)
			marker.GraphCompiled(1)
			marker.ResourcesConverged()

			require.NoError(t, r.updateStatus(context.Background(), work))

			got := &expv1alpha1.Graph{}
			require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "g"}, got))
			assert.Len(t, got.Status.Conditions, tc.wantConditionCount)
		})
	}
}

// TestReconcileResourcesDataPendingCondition asserts the reconciler
// distinguishes the two soft-error flavors via reason — DataPending
// for unresolved CEL refs, WaitingForReadiness for readyWhen=false.
// The error message reaching the marker decides which reason fires;
// errors.Is must traverse the fmt.Errorf wrapping chain.
func TestReconcileResourcesDataPendingCondition(t *testing.T) {
	tests := []struct {
		name       string
		applyErr   error
		wantReason string
	}{
		{
			name:       "data-pending-routes-to-DataPending",
			applyErr:   fmt.Errorf("apply %q: resolve: %w (%w)", "n", krotruntime.ErrDataPending, executor.ErrNotReady),
			wantReason: "DataPending",
		},
		{
			name:       "waiting-for-readiness-routes-to-WaitingForReadiness",
			applyErr:   fmt.Errorf("apply %q: %w (%w)", "n", krotruntime.ErrWaitingForReadiness, executor.ErrNotReady),
			wantReason: "WaitingForReadiness",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := graph("g", withFinalizer)
			cl := newClient(t, g)
			fc := &fakeCompiler{program: &compiler.Program{Nodes: map[string]*compiler.Node{"n": {}}}}
			r := &Reconciler{
				Client:   cl,
				Compiler: fc,
				Registry: registry.New(),
				Executor: &fakeExecutor{applyErr: tc.applyErr},
			}
			req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "g"}}
			res, err := r.Reconcile(context.Background(), req)
			require.NoError(t, err, "ErrNotReady should be a timed requeue, not an error")
			assert.Equal(t, notReadyRequeueAfter, res.RequeueAfter)

			got := &expv1alpha1.Graph{}
			require.NoError(t, cl.Get(context.Background(), req.NamespacedName, got))
			cond := findCondition(got.Status.Conditions, ResourcesConverged)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			require.NotNil(t, cond.Reason)
			assert.Equal(t, tc.wantReason, *cond.Reason)
		})
	}
}
