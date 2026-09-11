// Copyright 2026 The Kube Resource Orchestrator Authors.
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

package executor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

// TestClassifyRejection covers the validation-tolerance boundary and its reasons.
func TestClassifyRejection(t *testing.T) {
	t.Parallel()

	immutable := apierrors.NewInvalid(
		schema.GroupKind{Kind: "Service"}, "svc",
		field.ErrorList{field.Invalid(field.NewPath("spec", "clusterIP"), "10.0.0.9", "field is immutable")},
	)
	otherInvalid := apierrors.NewInvalid(
		schema.GroupKind{Kind: "ConfigMap"}, "cm",
		field.ErrorList{field.Invalid(field.NewPath("data"), "x", "too long")},
	)

	tests := []struct {
		name          string
		err           error
		wantReason    string
		wantPermanent bool
	}{
		{"immutable field", immutable, "field immutable", true},
		{"other invalid", otherInvalid, "invalid request", true},
		{"bad request", apierrors.NewBadRequest("nope"), "invalid request", true},
		{"conflict is transient", apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "cm", errors.New("rv")), "field-manager conflict, will retry", false},
		{"throttled is transient", apierrors.NewTooManyRequestsError("slow down"), "throttled, will retry", false},
		{"unavailable is transient", apierrors.NewServiceUnavailable("down"), "transient server error, will retry", false},
		{"unknown is transient", errors.New("something odd"), "rejected, will retry", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reason, permanent := classifyRejection(tc.err)
			assert.Equal(t, tc.wantReason, reason)
			assert.Equal(t, tc.wantPermanent, permanent)
		})
	}
}

// TestApply_OnToleratedRejectionHookFires verifies that a tolerated collection
// update-rejection on an already-existing item invokes OnToleratedRejection
// with the target identity + classification, while the node still converges
// (the hook is observational — it must not make Apply fail).
func TestApply_OnToleratedRejectionHookFires(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		err    error
		reason string
	}{
		{
			name: "immutable Invalid",
			err: apierrors.NewInvalid(schema.GroupKind{Kind: "ConfigMap"}, "cm-alpha",
				field.ErrorList{field.Invalid(field.NewPath("data"), "x", "field is immutable")}),
			reason: "field immutable",
		},
		{name: "BadRequest", err: apierrors.NewBadRequest("invalid request body"), reason: "invalid request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := fake.NewClientBuilder().WithScheme(newScheme(t)).
				WithObjects(liveCM("cm-alpha"), liveCM("cm-beta")).Build()
			cl := &concurrencyTrackingClient{Client: base, patchFunc: func(_ context.Context, obj client.Object) error {
				if obj.GetName() != "dependent" {
					return tc.err
				}
				return nil
			}}
			graph := collectionCMGraph()
			generator.WithTemplate("dependent", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "dependent"},
				"data":     map[string]any{"k": "${cm[0].data.k}"},
			})(graph)

			var mu sync.Mutex
			var got []ToleratedRejection
			s := NewSimple(cl)
			s.GateReadiness = true
			s.OnToleratedRejection = func(r ToleratedRejection) {
				mu.Lock()
				got = append(got, r)
				mu.Unlock()
			}

			res, err := s.Apply(context.Background(), compileAndBuild(t, graph), watchrouter.NoopWatcher{})
			require.NoError(t, err, "a tolerated update rejection must still converge")
			assert.Len(t, res.Applied, 3)
			assert.Empty(t, res.Unresolved)
			assert.Equal(t, map[string]any{"k": "old"}, getFakeCM(t, base, "dependent").Object["data"],
				"dependents must see the live value, not the rejected desired value")

			mu.Lock()
			defer mu.Unlock()
			require.Len(t, got, 2, "the hook must fire once per tolerated item")
			assert.ElementsMatch(t, []string{"cm-alpha", "cm-beta"}, []string{got[0].Name, got[1].Name})
			for _, r := range got {
				assert.Equal(t, "cm", r.NodeID)
				assert.Equal(t, "v1", r.APIVersion)
				assert.Equal(t, "ConfigMap", r.Kind)
				assert.Equal(t, "default", r.Namespace)
				assert.Equal(t, tc.reason, r.Reason)
				assert.Equal(t, tc.err.Error(), r.Cause)
			}
		})
	}
}

func TestApply_TransientUpdateRejectionHoldsNodeNotReady(t *testing.T) {
	t.Parallel()
	alpha, beta := liveCM("cm-alpha"), liveCM("cm-beta")
	alpha.SetUID("uid-alpha")
	beta.SetUID("uid-beta")
	base := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(alpha, beta).Build()
	rejection := apierrors.NewInternalError(errors.New("failed calling webhook: connection refused"))
	rejectUpdate := true
	cl := &concurrencyTrackingClient{Client: base, patchFunc: func(_ context.Context, obj client.Object) error {
		if rejectUpdate && obj.GetName() == "cm-alpha" {
			return rejection
		}
		return nil
	}}
	graph := collectionCMGraph()
	generator.WithTemplate("dependent", map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "dependent"},
		"data":     map[string]any{"k": "${cm[0].data.k}"},
	})(graph)
	s := NewSimple(cl)
	s.GateReadiness = true
	var hookCalls atomic.Int32
	s.OnToleratedRejection = func(ToleratedRejection) { hookCalls.Add(1) }
	rt := compileAndBuild(t, graph)

	res, err := s.Apply(context.Background(), rt, watchrouter.NoopWatcher{})
	assert.ErrorIs(t, err, ErrNotReady)
	assert.ErrorIs(t, err, rejection, "the underlying API error must be preserved")
	assert.ErrorContains(t, err, `collection "cm": 1 item(s) failed to apply`)
	assert.ErrorContains(t, err, "item default/cm-alpha")
	assert.ErrorContains(t, err, "failed calling webhook: connection refused")
	assert.NotErrorIs(t, err, ErrFieldManagerConflict)
	assert.Equal(t, []string{"cm", "dependent"}, res.Unresolved)
	wantMembers := []expv1alpha1.ManagedResource{
		{NodeID: "cm", APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "cm-alpha", UID: "uid-alpha"},
		{NodeID: "cm", APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "cm-beta", UID: "uid-beta"},
	}
	assert.Equal(t, wantMembers, res.Applied, "both live identities and UIDs must remain tracked")
	assert.Empty(t, rt.Node("cm").Observed(), "the failed collection must not publish a converged value")
	assert.False(t, cmExists(t, base, "dependent"), "dependents must wait for the rejected update")
	assert.Equal(t, map[string]any{"k": "old"}, getFakeCM(t, base, "cm-alpha").Object["data"])
	assert.Equal(t, map[string]any{"k": "new"}, getFakeCM(t, base, "cm-beta").Object["data"], "the healthy sibling still updates")
	assert.Zero(t, hookCalls.Load(), "retrying failures are not tolerated rejections")

	// Clear only the API failure; a later Apply uses the same, unedited graph.
	rejectUpdate = false
	res, err = s.Apply(context.Background(), compileAndBuild(t, graph), watchrouter.NoopWatcher{})
	require.NoError(t, err)
	assert.Empty(t, res.Unresolved)
	require.Len(t, res.Applied, 3)
	assert.Equal(t, wantMembers, res.Applied[:2], "recovery must retain the original member identities")
	for _, member := range wantMembers {
		live := getFakeCM(t, base, member.Name)
		assert.Equal(t, member.UID, string(live.GetUID()))
		assert.Equal(t, map[string]any{"k": "new"}, live.Object["data"])
	}
	assert.Equal(t, "dependent", res.Applied[2].Name)
	assert.Equal(t, map[string]any{"k": "new"}, getFakeCM(t, base, "dependent").Object["data"])
	assert.Zero(t, hookCalls.Load())
}

// TestApply_NoHookWhenNil confirms a nil hook is a safe no-op (the Graph
// controller path leaves it nil and relies on the log line).
func TestApply_NoHookWhenNil(t *testing.T) {
	t.Parallel()
	base := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(liveCM("cm-alpha"), liveCM("cm-beta")).Build()
	cl := &patchFailClient{Client: base, err: apierrors.NewInvalid(schema.GroupKind{Kind: "ConfigMap"}, "cm-alpha", nil)}

	s := NewSimple(cl) // OnToleratedRejection nil
	_, err := s.Apply(context.Background(),
		compileAndBuild(t, collectionCMGraph()), watchrouter.NoopWatcher{})
	require.NoError(t, err, "a nil hook must be a safe no-op")
}
