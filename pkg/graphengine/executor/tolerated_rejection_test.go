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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

// TestClassifyRejection covers the permanent-vs-transient classification that
// enriches the tolerated-update-rejection signal (finding 886, Opt 2). The axis
// is NOT "immutable vs still-reconciling" (a reconciling object is an accepted
// write, never an error here) but "permanent vs transient rejection".
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

	// Both members already exist, so every SSA is an update; reject with an
	// immutable-field Invalid so the tolerate path fires.
	base := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(liveCM("cm-alpha"), liveCM("cm-beta")).Build()
	immutable := apierrors.NewInvalid(
		schema.GroupKind{Kind: "ConfigMap"}, "cm-alpha",
		field.ErrorList{field.Invalid(field.NewPath("data"), "x", "field is immutable")},
	)
	cl := &patchFailClient{Client: base, err: immutable}

	var mu sync.Mutex
	var got []ToleratedRejection
	s := NewSimple(cl)
	s.OnToleratedRejection = func(r ToleratedRejection) {
		mu.Lock()
		got = append(got, r)
		mu.Unlock()
	}

	res, err := s.Apply(context.Background(),
		compileAndBuild(t, collectionCMGraph()), watchrouter.NoopWatcher{})

	require.NoError(t, err, "a tolerated update-rejection must not fail Apply (the node converges)")
	// Both items converge into Applied (live identities recorded).
	assert.Len(t, res.Applied, 2)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, 2, "the hook must fire once per tolerated item")
	for _, r := range got {
		assert.Equal(t, "v1", r.APIVersion)
		assert.Equal(t, "ConfigMap", r.Kind)
		assert.Equal(t, "default", r.Namespace)
		assert.Equal(t, "field immutable", r.Reason)
		assert.True(t, r.Permanent, "an immutable-field rejection is permanent")
		assert.Contains(t, r.Cause, "immutable")
	}
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
