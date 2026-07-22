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

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	metafake "k8s.io/client-go/metadata/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/schemawatcher"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

// TestNoopSchemaSubscription pins the inert fallback used when no
// SchemaWatcher is wired. Every method is a no-op that must not panic.
func TestNoopSchemaSubscription(t *testing.T) {
	var sub schemawatcher.Subscription = noopSchemaSubscription{}
	cases := []struct {
		name string
		call func()
	}{
		{name: "Track", call: func() { sub.Track(schema.GroupKind{Group: "apps", Kind: "Deployment"}) }},
		{name: "TrackDynamic", call: func() { sub.TrackDynamic() }},
		{name: "Done-commit", call: func() { sub.Done(true) }},
		{name: "Done-abort", call: func() { sub.Done(false) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.NotPanics(t, tc.call)
		})
	}
}

// TestWatcherForAndSchemaSubFor covers both arms of the per-Graph handle
// selectors: the noop fallback when nothing is wired, and the real
// delegate when a Router / SchemaWatcher is present.
func TestWatcherForAndSchemaSubFor(t *testing.T) {
	key := client.ObjectKey{Namespace: "default", Name: "g"}

	t.Run("nil-router-yields-noop-watcher", func(t *testing.T) {
		r := &Reconciler{}
		w := r.watcherFor(key)
		_, isNoop := w.(watchrouter.NoopWatcher)
		assert.True(t, isNoop, "nil Router must return NoopWatcher")
	})

	t.Run("wired-router-yields-real-watcher", func(t *testing.T) {
		mc := metafake.NewSimpleMetadataClient(metafake.NewTestScheme())
		router := watchrouter.NewRouter(logr.Discard(), watchrouter.Config{EventBuffer: 8}, mc)
		r := &Reconciler{Router: router}
		w := r.watcherFor(key)
		_, isNoop := w.(watchrouter.NoopWatcher)
		assert.False(t, isNoop, "wired Router must return a real graph watcher")
		assert.NotNil(t, w)
	})

	t.Run("nil-schemawatcher-yields-noop-subscription", func(t *testing.T) {
		r := &Reconciler{}
		sub := r.schemaSubFor(key)
		_, isNoop := sub.(noopSchemaSubscription)
		assert.True(t, isNoop, "nil SchemaWatcher must return noopSchemaSubscription")
	})

	t.Run("wired-schemawatcher-yields-real-subscription", func(t *testing.T) {
		sw := schemawatcher.New(logr.Discard(), schemawatcher.Config{EventBuffer: 8})
		r := &Reconciler{SchemaWatcher: sw}
		sub := r.schemaSubFor(key)
		_, isNoop := sub.(noopSchemaSubscription)
		assert.False(t, isNoop, "wired SchemaWatcher must return a real subscription")
		assert.NotNil(t, sub)
	})
}

// TestUnionManagedResourcesAppliedDup covers the dedup-within-applied
// branch: two applied entries sharing an identity collapse to one, with
// previous-first ordering preserved.
func TestUnionManagedResourcesAppliedDup(t *testing.T) {
	cases := []struct {
		name     string
		previous []expv1alpha1.ManagedResource
		applied  []expv1alpha1.ManagedResource
		want     []expv1alpha1.ManagedResource
	}{
		{
			name:     "dup-within-applied-collapses",
			previous: []expv1alpha1.ManagedResource{r("n1", "ConfigMap", "a")},
			applied: []expv1alpha1.ManagedResource{
				r("n2", "ConfigMap", "b"),
				r("n2", "ConfigMap", "b"), // duplicate within applied
				r("n1", "ConfigMap", "a"), // also dup of previous
			},
			want: []expv1alpha1.ManagedResource{
				r("n1", "ConfigMap", "a"),
				r("n2", "ConfigMap", "b"),
			},
		},
		{
			name: "dup-within-previous-collapses",
			previous: []expv1alpha1.ManagedResource{
				r("n1", "ConfigMap", "a"),
				r("n1", "ConfigMap", "a"), // duplicate within previous
			},
			applied: []expv1alpha1.ManagedResource{r("n2", "ConfigMap", "b")},
			want: []expv1alpha1.ManagedResource{
				r("n1", "ConfigMap", "a"),
				r("n2", "ConfigMap", "b"),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, unionManagedResources(tc.previous, tc.applied))
		})
	}
}

// TestUpdateStatusRefetchError covers the error arm of updateStatus when
// the live-object refetch fails: the error must be wrapped and surfaced.
func TestUpdateStatusRefetchError(t *testing.T) {
	g := graph("g", withFinalizer)
	cl := newClient(t, g)
	wrapped := &patchErrClient{Client: cl, getErr: errFetch}
	r := &Reconciler{Client: wrapped, Registry: registry.New()}

	work := graph("g", withFinalizer)
	NewConditionsMarkerFor(work).GraphCompiled(1)
	err := r.updateStatus(context.Background(), work)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refetch graph")
}

// TestReconcilePrunesStaleResources drives the clean-apply prune branch
// of reconcileGraph: a previously-tracked resource absent from this
// cycle's Applied set is a prune candidate, so Executor.Delete fires and
// status shrinks to the new set.
func TestReconcilePrunesStaleResources(t *testing.T) {
	stale := r("n", "ConfigMap", "old")
	fresh := r("n", "ConfigMap", "new")

	g := graph("g", withFinalizer)
	g.Status.ManagedResources = []expv1alpha1.ManagedResource{stale}
	cl := newClient(t, g)

	exec := &fakeExecutor{applyResult: executor.ApplyResult{
		Applied: []expv1alpha1.ManagedResource{fresh},
	}}
	r := &Reconciler{
		Client:   cl,
		Compiler: &fakeCompiler{program: &compiler.Program{Nodes: map[string]*compiler.Node{"n": {}}}},
		Registry: registry.New(),
		Executor: exec,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "g"}}
	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	// The stale resource must have been handed to Delete.
	require.Len(t, exec.deleteCalls, 1)
	assert.Equal(t, []expv1alpha1.ManagedResource{stale}, exec.deleteCalls[0])

	got := &expv1alpha1.Graph{}
	require.NoError(t, cl.Get(context.Background(), req.NamespacedName, got))
	assert.Equal(t, []expv1alpha1.ManagedResource{fresh}, got.Status.ManagedResources)
}

// TestReconcilePruneFailureKeepsUnion covers the prune-error arm: when
// Delete fails for prune candidates, status must widen to the union of
// previous and applied (not shrink) and the reconcile surfaces the error.
func TestReconcilePruneFailureKeepsUnion(t *testing.T) {
	stale := r("n", "ConfigMap", "old")
	fresh := r("n", "ConfigMap", "new")

	g := graph("g", withFinalizer)
	g.Status.ManagedResources = []expv1alpha1.ManagedResource{stale}
	cl := newClient(t, g)

	exec := &fakeExecutor{
		applyResult: executor.ApplyResult{Applied: []expv1alpha1.ManagedResource{fresh}},
		deleteErr:   errFetch,
	}
	r := &Reconciler{
		Client:   cl,
		Compiler: &fakeCompiler{program: &compiler.Program{Nodes: map[string]*compiler.Node{"n": {}}}},
		Registry: registry.New(),
		Executor: exec,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "g"}}
	_, err := r.Reconcile(context.Background(), req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prune")

	got := &expv1alpha1.Graph{}
	require.NoError(t, cl.Get(context.Background(), req.NamespacedName, got))
	assert.ElementsMatch(t, []expv1alpha1.ManagedResource{stale, fresh}, got.Status.ManagedResources)
}

var errFetch = errSentinel("boom")

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

// keep runtime import referenced (metafake scheme helpers pull it in
// transitively, but guard against an unused import if that changes).
var _ = runtime.NewScheme
