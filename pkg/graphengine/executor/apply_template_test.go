// Copyright 2026 The Kubernetes Authors.
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
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

// patchFailClient delegates everything to the embedded client except SSA
// apply, so a test can distinguish create-vs-update failure handling: Get
// still reports whether the object exists.
type patchFailClient struct {
	client.Client
	err error
}

func (p *patchFailClient) Patch(
	context.Context, client.Object, client.Patch, ...client.PatchOption,
) error {
	return p.err
}

// failWatcher rejects every watch declaration.
type failWatcher struct{ err error }

func (f failWatcher) Watch(watchrouter.WatchRequest) error { return f.err }
func (f failWatcher) Done(bool)                            {}

// terminatingCM builds a ConfigMap that is mid-deletion. The fake client
// requires a finalizer alongside deletionTimestamp, which mirrors reality: an
// object only lingers in a terminating state because something is finalizing it.
func terminatingCM(name string) *unstructured.Unstructured {
	now := metav1.Now()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": "default"},
		"data":     map[string]any{"k": "old"},
	}}
	obj.SetFinalizers([]string{"example.com/hold"})
	obj.SetDeletionTimestamp(&now)
	return obj
}

func liveCM(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": "default"},
		"data":     map[string]any{"k": "old"},
	}}
}

func scalarCMGraph(name string) *expv1alpha1.Graph {
	return generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithTemplate("cm", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": name},
			"data":     map[string]any{"k": "new"},
		}),
	)
}

// collectionCMGraph expands to cm-alpha and cm-beta via forEach.
func collectionCMGraph() *expv1alpha1.Graph {
	return generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithDef("src", map[string]any{"names": []any{"alpha", "beta"}}),
		generator.WithTemplate("cm", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "${'cm-' + n}"},
			"data":     map[string]any{"k": "new"},
		}, generator.ForEachDim("n", "${src.names}")),
	)
}

// kro must not re-apply over an object that is being deleted: the write would
// either fail or resurrect fields on a doomed object. A terminating scalar
// template surfaces the distinguishable ResourceDeleting signal, which also
// satisfies ErrNotReady so the reconciler requeues and gates dependents
// instead of failing.
func TestSimple_ApplyTemplate_TerminatingScalar(t *testing.T) {
	t.Parallel()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(terminatingCM("doomed")).Build()

	res, err := NewSimple(cl).Apply(context.Background(),
		compileAndBuild(t, scalarCMGraph("doomed")), watchrouter.NoopWatcher{})

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrResourceDeleting),
		"a terminating live object must be distinguishable, got %v", err)
	assert.True(t, errors.Is(err, ErrNotReady),
		"and must requeue rather than fail the reconcile")
	assert.Contains(t, res.Unresolved, "cm",
		"an unresolved identity must withhold prune for this node")

	var target *ResourceDeletingError
	require.True(t, errors.As(err, &target))
	assert.Equal(t, "doomed", target.Name)
}

// In a collection, one terminating item gates the whole node but must not stop
// its siblings from applying — otherwise a single stuck item would starve every
// other member of its watches and identities.
func TestSimple_ApplyTemplate_TerminatingCollectionItemStillAppliesSiblings(t *testing.T) {
	t.Parallel()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(terminatingCM("cm-alpha")).Build()

	res, err := NewSimple(cl).Apply(context.Background(),
		compileAndBuild(t, collectionCMGraph()), watchrouter.NoopWatcher{})

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrResourceDeleting))
	assert.True(t, errors.Is(err, ErrNotReady))

	assert.True(t, cmExists(t, cl, "cm-beta"),
		"a sibling of a terminating item must still be applied")

	names := make([]string, 0, len(res.Applied))
	for _, a := range res.Applied {
		names = append(names, a.Name)
	}
	assert.Contains(t, names, "cm-beta", "the applied sibling must be tracked")
	assert.NotContains(t, names, "cm-alpha",
		"a terminating item must not be advertised as applied")
}

// A watch that cannot be registered must NOT abort the apply (issue #17): the
// pre-drift base behavior applied everything and only lost drift detection.
// The object is still applied, Apply returns no error, and the node is not
// recorded Unresolved.
func TestSimple_ApplyTemplate_WatchRegistrationFailureIsSoftFail(t *testing.T) {
	t.Parallel()

	t.Run("scalar", func(t *testing.T) {
		t.Parallel()
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		res, err := NewSimple(cl).Apply(context.Background(),
			compileAndBuild(t, scalarCMGraph("cm")),
			failWatcher{err: errors.New("informer unavailable")})

		require.NoError(t, err,
			"a watch registration failure must not abort the apply")
		assert.True(t, cmExists(t, cl, "cm"),
			"the object must still be applied even though its drift watch failed")
		assert.NotContains(t, res.Unresolved, "cm",
			"a lost drift watch must not mark the node unresolved")
		names := make([]string, 0, len(res.Applied))
		for _, a := range res.Applied {
			names = append(names, a.Name)
		}
		assert.Contains(t, names, "cm", "the applied object must still be tracked")
	})

	t.Run("collection", func(t *testing.T) {
		t.Parallel()
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		res, err := NewSimple(cl).Apply(context.Background(),
			compileAndBuild(t, collectionCMGraph()),
			failWatcher{err: errors.New("informer unavailable")})

		require.NoError(t, err,
			"a collection watch registration failure must not abort the apply")
		assert.True(t, cmExists(t, cl, "cm-alpha"),
			"collection items must still be applied even though the drift watch failed")
		assert.True(t, cmExists(t, cl, "cm-beta"))
		assert.Empty(t, res.Unresolved,
			"a lost collection drift watch must not mark the node unresolved")
	})
}

// Validation rejections are tolerated only on existing items; other failures
// must retry without losing live identities.
func TestSimple_ApplyTemplate_CollectionApplyTolerance(t *testing.T) {
	t.Parallel()

	t.Run("an update conflict retains live identities and retries", func(t *testing.T) {
		t.Parallel()
		// This is an ordinary optimistic-concurrency 409, not an SSA ownership conflict.
		base := fake.NewClientBuilder().WithScheme(newScheme(t)).
			WithObjects(liveCM("cm-alpha"), liveCM("cm-beta")).Build()
		cl := &patchFailClient{Client: base, err: apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "cm-alpha", errors.New("resource version changed"))}

		res, err := NewSimple(cl).Apply(context.Background(),
			compileAndBuild(t, collectionCMGraph()), watchrouter.NoopWatcher{})

		require.ErrorIs(t, err, ErrNotReady)
		assert.NotErrorIs(t, err, ErrFieldManagerConflict)
		assert.ErrorContains(t, err, "resource version changed")
		assert.Contains(t, res.Unresolved, "cm")
		names := make([]string, 0, len(res.Applied))
		for _, a := range res.Applied {
			names = append(names, a.Name)
		}
		assert.ElementsMatch(t, []string{"cm-alpha", "cm-beta"}, names,
			"existing objects keep their tracked identities")
	})

	t.Run("a permanent update rejection on an existing object is tolerated", func(t *testing.T) {
		t.Parallel()
		// Invalid updates on existing objects remain tolerated, including immutable fields.
		base := fake.NewClientBuilder().WithScheme(newScheme(t)).
			WithObjects(liveCM("cm-alpha"), liveCM("cm-beta")).Build()
		cl := &patchFailClient{Client: base, err: apierrors.NewInvalid(schema.GroupKind{Kind: "ConfigMap"}, "cm-alpha", nil)}

		res, err := NewSimple(cl).Apply(context.Background(),
			compileAndBuild(t, collectionCMGraph()), watchrouter.NoopWatcher{})

		require.NoError(t, err,
			"a permanent update rejection on existing items must be tolerated so the node converges")
		names := make([]string, 0, len(res.Applied))
		for _, a := range res.Applied {
			names = append(names, a.Name)
		}
		assert.ElementsMatch(t, []string{"cm-alpha", "cm-beta"}, names,
			"existing objects keep their tracked identities even when the update is rejected")
	})

	t.Run("a failed create holds the collection soft not-ready", func(t *testing.T) {
		t.Parallel()
		// Neither member exists, so every SSA is a create. A failed create
		// means the resource is absent: record the failure, keep going, and
		// hold the node not-ready so the reconcile requeues rather than
		// hard-failing.
		base := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		cl := &patchFailClient{Client: base, err: errors.New("quota exceeded")}

		res, err := NewSimple(cl).Apply(context.Background(),
			compileAndBuild(t, collectionCMGraph()), watchrouter.NoopWatcher{})

		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrNotReady),
			"a failed create must requeue, never hard-abort the walk, got %v", err)
		assert.Contains(t, err.Error(), "quota exceeded",
			"the underlying cause must reach the condition message")
		assert.Contains(t, err.Error(), "2 item(s)",
			"every failing item is reported, not just the first")
		assert.Empty(t, res.Applied,
			"nothing landed, so nothing may be advertised as applied")
	})

	t.Run("a malformed create (Invalid) fails hard instead of requeuing forever", func(t *testing.T) {
		t.Parallel()
		// Neither member exists, so every SSA is a create. A create rejected as
		// Invalid can never succeed (the rendered object is malformed), so it
		// must surface as a HARD error — the node fails fast rather than holding
		// soft not-ready and requeuing an unfixable create forever.
		base := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		cl := &patchFailClient{Client: base, err: apierrors.NewInvalid(schema.GroupKind{Kind: "ConfigMap"}, "cm-alpha", nil)}

		res, err := NewSimple(cl).Apply(context.Background(),
			compileAndBuild(t, collectionCMGraph()), watchrouter.NoopWatcher{})

		require.Error(t, err)
		assert.False(t, errors.Is(err, ErrNotReady),
			"a malformed (Invalid) create must be a hard error, not a soft requeue, got %v", err)
		assert.Empty(t, res.Applied,
			"nothing landed, so nothing may be advertised as applied")
	})
}

// A tolerated validation rejection publishes the live value without an error.
func TestCollectionApplyState_UpdateRejectedIsSilent(t *testing.T) {
	t.Parallel()

	st := &collectionApplyState{
		results:      make([]*expv1alpha1.ManagedResource, 2),
		hardErrors:   make([]error, 2),
		itemFailures: make([]error, 2),
	}

	current := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "cm-alpha", "namespace": "default"},
	}}
	desired := []*unstructured.Unstructured{{}, {}}

	st.recordUpdateRejected(0, expv1alpha1.ManagedResource{Name: "cm-alpha", Namespace: "default"}, desired, current)

	// Identity is tracked — the item converges into Applied.
	applied := st.appliedResources()
	require.Len(t, applied, 1, "the rejected-but-present item keeps its tracked identity")
	assert.Equal(t, "cm-alpha", applied[0].Name)

	// The desired slot is replaced with the live object so downstream scope
	// sees what actually landed.
	assert.Same(t, current, desired[0], "the desired slot is replaced with the live object")

	// The tolerated rejection is silent: neither a soft item failure nor a
	// hard error is recorded, so nothing is surfaced up the walk.
	assert.NoError(t, st.softError("node"),
		"a tolerated update-rejection must not be surfaced as a soft not-ready error")
	assert.NoError(t, st.hardError(),
		"a tolerated update-rejection must not be surfaced as a hard error")
}

type concurrencyTrackingClient struct {
	client.Client
	active     atomic.Int32
	maxSeen    atomic.Int32
	patchDelay time.Duration
	patchFunc  func(ctx context.Context, obj client.Object) error
}

func (c *concurrencyTrackingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	curr := c.active.Add(1)
	for {
		oldMax := c.maxSeen.Load()
		if curr <= oldMax || c.maxSeen.CompareAndSwap(oldMax, curr) {
			break
		}
	}
	if c.patchDelay > 0 {
		time.Sleep(c.patchDelay)
	}
	defer c.active.Add(-1)

	if c.patchFunc != nil {
		if err := c.patchFunc(ctx, obj); err != nil {
			return err
		}
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

type shuffledPatchClient struct {
	client.Client
}

func (s *shuffledPatchClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	name := obj.GetName()
	// Artificially shuffle completion time by sleeping a pseudo-random tiny duration keyed by name.
	var hash uint32 = 2166136261
	for i := 0; i < len(name); i++ {
		hash ^= uint32(name[i])
		hash *= 16777619
	}
	delay := time.Duration(hash%15+1) * time.Millisecond
	time.Sleep(delay)
	return s.Client.Patch(ctx, obj, patch, opts...)
}

func TestSimple_ApplyTemplate_CollectionDeterministicAppliedOrder(t *testing.T) {
	t.Parallel()

	const count = 30
	graph := largeCollectionGraph(count)

	var firstRunOrder []string

	// Run repeatedly to verify deterministic ordering across independent reconciles
	for run := range 5 {
		base := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		cl := &shuffledPatchClient{Client: base}
		ex := NewSimple(cl)

		res, err := ex.Apply(context.Background(),
			compileAndBuild(t, graph), watchrouter.NoopWatcher{})
		require.NoError(t, err)
		require.Len(t, res.Applied, count)

		currentRunOrder := make([]string, count)
		for i, a := range res.Applied {
			expectedName := fmt.Sprintf("cm-item-%02d", i)
			assert.Equal(t, expectedName, a.Name, "item at index %d must match collection index order", i)
			currentRunOrder[i] = a.Name
		}

		if run == 0 {
			firstRunOrder = currentRunOrder
		} else {
			assert.Equal(t, firstRunOrder, currentRunOrder, "Applied order must be identical across runs")
		}
	}
}

func largeCollectionGraph(count int) *expv1alpha1.Graph {
	names := make([]any, count)
	for i := range count {
		names[i] = fmt.Sprintf("item-%02d", i)
	}
	return generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithDef("src", map[string]any{"names": names}),
		generator.WithTemplate("cm", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "${'cm-' + n}"},
			"data":     map[string]any{"k": "new"},
		}, generator.ForEachDim("n", "${src.names}")),
	)
}

func TestSimple_ApplyTemplate_LargeCollectionParallel(t *testing.T) {
	t.Parallel()

	const count = 60

	t.Run("default concurrency applies all items in parallel", func(t *testing.T) {
		t.Parallel()
		base := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		cl := &concurrencyTrackingClient{
			Client:     base,
			patchDelay: 2 * time.Millisecond,
		}

		ex := NewSimple(cl)
		res, err := ex.Apply(context.Background(),
			compileAndBuild(t, largeCollectionGraph(count)), watchrouter.NoopWatcher{})

		require.NoError(t, err)
		assert.Len(t, res.Applied, count, "every collection item must be recorded as applied")

		names := make([]string, 0, len(res.Applied))
		for _, a := range res.Applied {
			names = append(names, a.Name)
			assert.True(t, cmExists(t, base, a.Name), "item %q must exist in cluster", a.Name)
		}

		for i := range count {
			expected := fmt.Sprintf("cm-item-%02d", i)
			assert.Contains(t, names, expected)
		}

		maxObserved := cl.maxSeen.Load()
		assert.Greater(t, maxObserved, int32(1), "observed max concurrency must be > 1")
		assert.LessOrEqual(t, maxObserved, int32(defaultApplyConcurrency), "observed concurrency must not exceed default bound")
	})

	t.Run("explicit ApplyConcurrency bounds parallel workers", func(t *testing.T) {
		t.Parallel()
		const bound = 5
		base := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		cl := &concurrencyTrackingClient{
			Client:     base,
			patchDelay: 2 * time.Millisecond,
		}

		ex := NewSimple(cl)
		ex.ApplyConcurrency = bound
		res, err := ex.Apply(context.Background(),
			compileAndBuild(t, largeCollectionGraph(count)), watchrouter.NoopWatcher{})

		require.NoError(t, err)
		assert.Len(t, res.Applied, count)
		maxObserved := cl.maxSeen.Load()
		assert.Greater(t, maxObserved, int32(1), "observed max concurrency must be > 1")
		assert.LessOrEqual(t, maxObserved, int32(bound), "observed concurrency must not exceed configured bound")
	})
}

func TestSimple_ApplyTemplate_LargeCollectionTolerance(t *testing.T) {
	t.Parallel()

	const count = 60
	// 00..09: pre-existing, fail on update (soft ErrNotReady, live recorded)
	// 10..19: absent, fail on create (tolerated, recorded in itemFailures, soft ErrNotReady)
	// 20..59: absent, succeed on create (applied)

	seeded := make([]client.Object, 0, 10)
	for i := range 10 {
		seeded = append(seeded, liveCM(fmt.Sprintf("cm-item-%02d", i)))
	}

	base := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(seeded...).Build()
	cl := &concurrencyTrackingClient{
		Client: base,
		patchFunc: func(ctx context.Context, obj client.Object) error {
			name := obj.GetName()
			if strings.HasPrefix(name, "cm-item-0") {
				return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, name, errors.New("resource version changed"))
			}
			if strings.HasPrefix(name, "cm-item-1") {
				return errors.New("quota exceeded")
			}
			return nil
		},
	}

	ex := NewSimple(cl)
	res, err := ex.Apply(context.Background(),
		compileAndBuild(t, largeCollectionGraph(count)), watchrouter.NoopWatcher{})

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotReady), "failing creates and transient updates must produce soft ErrNotReady")
	assert.Contains(t, err.Error(), "20 item(s) failed to apply")
	assert.Contains(t, err.Error(), "resource version changed")
	assert.Contains(t, err.Error(), "quota exceeded")
	assert.Contains(t, res.Unresolved, "cm")

	// 10 existing + 40 newly created = 50 items tracked in Applied
	assert.Len(t, res.Applied, 50)

	appliedNames := make(map[string]bool, len(res.Applied))
	for _, a := range res.Applied {
		appliedNames[a.Name] = true
	}

	for i := range 10 {
		name := fmt.Sprintf("cm-item-%02d", i)
		assert.True(t, appliedNames[name], "pre-existing item with rejected update must be in Applied: %s", name)
	}
	for i := 10; i < 20; i++ {
		name := fmt.Sprintf("cm-item-%02d", i)
		assert.False(t, appliedNames[name], "failed create item must not be in Applied: %s", name)
	}
	for i := 20; i < count; i++ {
		name := fmt.Sprintf("cm-item-%02d", i)
		assert.True(t, appliedNames[name], "successful create item must be in Applied: %s", name)
		assert.True(t, cmExists(t, base, name), "successful create item must exist in cluster: %s", name)
	}
}

func TestSimple_ApplyTemplate_EmptyCollection(t *testing.T) {
	t.Parallel()
	g := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithDef("src", map[string]any{"names": []any{}}),
		generator.WithTemplate("cm", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "${'cm-' + n}"},
			"data":     map[string]any{"k": "v"},
		}, generator.ForEachDim("n", "${src.names}")),
	)

	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	res, err := NewSimple(cl).Apply(context.Background(),
		compileAndBuild(t, g), watchrouter.NoopWatcher{})

	require.NoError(t, err)
	assert.Empty(t, res.Applied)
}

func TestSimple_ApplyTemplate_CollectionHardErrors(t *testing.T) {
	t.Parallel()

	t.Run("hard GET error aborts apply and returns error", func(t *testing.T) {
		t.Parallel()
		base := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		cl := &getFailClient{Client: base, err: errors.New("etcd partition")}

		_, err := NewSimple(cl).Apply(context.Background(),
			compileAndBuild(t, collectionCMGraph()), watchrouter.NoopWatcher{})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "etcd partition")
		assert.False(t, errors.Is(err, ErrNotReady))
	})

	t.Run("namespaced resource with empty namespace on cluster-scoped instance returns hard error", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			// Cluster-scoped graph (empty namespace)
			generator.WithDef("src", map[string]any{"names": []any{"alpha", "beta"}}),
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${'cm-' + n}"},
				"data":     map[string]any{"k": "v"},
			}, generator.ForEachDim("n", "${src.names}")),
		)

		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		_, err := NewSimple(cl).Apply(context.Background(),
			compileAndBuild(t, g), watchrouter.NoopWatcher{})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "must set metadata.namespace when the instance is cluster-scoped")
		assert.False(t, errors.Is(err, ErrNotReady))
	})

	t.Run("Invalid updates are tolerated but Forbidden updates retry", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("src", map[string]any{"names": []any{"0", "1", "2"}}),
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${'cm-' + n}"},
				"data":     map[string]any{"k": "v"},
			}, generator.ForEachDim("n", "${src.names}")),
		)

		// All three items already exist. Invalid is tolerated, Forbidden holds
		// the node not-ready, and the healthy sibling still applies.
		base := fake.NewClientBuilder().WithScheme(newScheme(t)).
			WithObjects(liveCM("cm-0"), liveCM("cm-1"), liveCM("cm-2")).Build()
		cl := &concurrencyTrackingClient{
			Client: base,
			patchFunc: func(ctx context.Context, obj client.Object) error {
				name := obj.GetName()
				if name == "cm-0" {
					return apierrors.NewInvalid(schema.GroupKind{Kind: "ConfigMap"}, "cm-0", nil)
				}
				if name == "cm-2" {
					return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "cm-2", errors.New("forbidden mutation"))
				}
				return nil
			},
		}

		res, err := NewSimple(cl).Apply(context.Background(),
			compileAndBuild(t, g), watchrouter.NoopWatcher{})

		require.ErrorIs(t, err, ErrNotReady)
		assert.ErrorContains(t, err, "1 item(s) failed to apply")
		assert.ErrorContains(t, err, "item default/cm-2")
		assert.ErrorContains(t, err, "forbidden mutation")
		assert.Contains(t, res.Unresolved, "cm")
		names := make([]string, 0, len(res.Applied))
		for _, a := range res.Applied {
			names = append(names, a.Name)
		}
		assert.ElementsMatch(t, []string{"cm-0", "cm-1", "cm-2"}, names,
			"every existing item keeps its tracked identity")
	})
}
