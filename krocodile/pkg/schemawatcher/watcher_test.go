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

package schemawatcher

import (
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// --- Test fixtures ----------------------------------------------------

// fakeGraphInvalidator records every key handed to Delete.
type fakeGraphInvalidator struct {
	mu    sync.Mutex
	calls []client.ObjectKey
}

func (f *fakeGraphInvalidator) Delete(key client.ObjectKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, key)
}

func (f *fakeGraphInvalidator) snapshot() []client.ObjectKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]client.ObjectKey(nil), f.calls...)
}

// fakeSchemaInvalidator records every GK handed to InvalidateSchema.
type fakeSchemaInvalidator struct {
	mu    sync.Mutex
	calls []schema.GroupKind
}

func (f *fakeSchemaInvalidator) InvalidateSchema(gk schema.GroupKind) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, gk)
}

func (f *fakeSchemaInvalidator) snapshot() []schema.GroupKind {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]schema.GroupKind(nil), f.calls...)
}

// newTestWatcher builds a SchemaWatcher wired to fakes for the tests
// that don't care about the actual informer side. The Cache is nil —
// these tests poke onAdd / onUpdate / onDelete directly.
func newTestWatcher(t *testing.T) (*SchemaWatcher, *fakeGraphInvalidator, *fakeSchemaInvalidator) {
	t.Helper()
	gi := &fakeGraphInvalidator{}
	si := &fakeSchemaInvalidator{}
	sw := New(logr.Discard(), Config{
		Graphs:      gi,
		Schemas:     si,
		EventBuffer: 16,
	})
	return sw, gi, si
}

// makeCRD builds a minimal CRD with the given GroupKind and schema
// content (the schema is just a marker — when it changes, the hash
// changes).
func makeCRD(group, kind, schemaTag string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: kind + "s." + group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: kind, Plural: kind + "s"},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    "v1",
				Served:  true,
				Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Description: schemaTag,
					},
				},
			}},
		},
	}
}

// drainEvents reads everything queued on the events chan without
// blocking. Returns the Graph keys in order of arrival.
func drainEvents(sw *SchemaWatcher, timeout time.Duration) []client.ObjectKey {
	var out []client.ObjectKey
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-sw.events:
			out = append(out, client.ObjectKey{
				Namespace: ev.Object.GetNamespace(),
				Name:      ev.Object.GetName(),
			})
		case <-deadline:
			return out
		}
	}
}

// --- Subscribe API ---------------------------------------------------

// TestSubscribeCycle covers the Track / TrackDynamic / Done(commit)
// machinery: index population, removal on commit, abort preserving
// previous, double-Track idempotence.
func TestSubscribeCycle(t *testing.T) {
	graphA := client.ObjectKey{Namespace: "ns", Name: "a"}
	gkDeploy := schema.GroupKind{Group: "apps", Kind: "Deployment"}
	gkCfg := schema.GroupKind{Group: "", Kind: "ConfigMap"}

	t.Run("commit-installs-static-entries", func(t *testing.T) {
		sw, _, _ := newTestWatcher(t)
		s := sw.ForGraph(graphA)
		s.Track(gkDeploy)
		s.Track(gkCfg)
		s.Done(true)
		assert.ElementsMatch(t, []client.ObjectKey{graphA}, sw.GraphsForGroupKind(gkDeploy))
		assert.ElementsMatch(t, []client.ObjectKey{graphA}, sw.GraphsForGroupKind(gkCfg))
		assert.Empty(t, sw.DynamicGraphs())
	})

	t.Run("dynamic-track-installs-only-into-dynamic-set", func(t *testing.T) {
		sw, _, _ := newTestWatcher(t)
		s := sw.ForGraph(graphA)
		s.TrackDynamic()
		s.Done(true)
		assert.Empty(t, sw.GraphsForGroupKind(gkDeploy))
		assert.ElementsMatch(t, []client.ObjectKey{graphA}, sw.DynamicGraphs())
	})

	t.Run("commit-removes-stale-entries", func(t *testing.T) {
		sw, _, _ := newTestWatcher(t)

		// Cycle 1: commit Deployment.
		s := sw.ForGraph(graphA)
		s.Track(gkDeploy)
		s.Done(true)

		// Cycle 2: stop tracking Deployment, start tracking ConfigMap.
		s2 := sw.ForGraph(graphA)
		s2.Track(gkCfg)
		s2.Done(true)

		assert.Empty(t, sw.GraphsForGroupKind(gkDeploy), "stale GK should be evicted")
		assert.ElementsMatch(t, []client.ObjectKey{graphA}, sw.GraphsForGroupKind(gkCfg))
	})

	t.Run("abort-discards-current-keeps-previous", func(t *testing.T) {
		sw, _, _ := newTestWatcher(t)

		s := sw.ForGraph(graphA)
		s.Track(gkDeploy)
		s.Done(true)

		s2 := sw.ForGraph(graphA)
		s2.Track(gkCfg) // in-flight
		s2.Done(false)  // discard

		assert.ElementsMatch(t, []client.ObjectKey{graphA}, sw.GraphsForGroupKind(gkDeploy),
			"previous committed set must survive an abort")
		assert.Empty(t, sw.GraphsForGroupKind(gkCfg),
			"in-flight entry must be rolled back on abort")
	})

	t.Run("double-track-is-idempotent", func(t *testing.T) {
		sw, _, _ := newTestWatcher(t)
		s := sw.ForGraph(graphA)
		s.Track(gkDeploy)
		s.Track(gkDeploy)
		s.Track(gkDeploy)
		s.Done(true)
		// One Graph, one entry.
		assert.ElementsMatch(t, []client.ObjectKey{graphA}, sw.GraphsForGroupKind(gkDeploy))
	})

	t.Run("dynamic-toggle-off-removes-from-set", func(t *testing.T) {
		sw, _, _ := newTestWatcher(t)

		// Cycle 1: dynamic.
		s := sw.ForGraph(graphA)
		s.TrackDynamic()
		s.Done(true)
		assert.ElementsMatch(t, []client.ObjectKey{graphA}, sw.DynamicGraphs())

		// Cycle 2: static-only.
		s2 := sw.ForGraph(graphA)
		s2.Track(gkDeploy)
		s2.Done(true)
		assert.Empty(t, sw.DynamicGraphs(), "dynamic flag should drop after a commit without TrackDynamic")
	})
}

// TestRemoveGraph clears all subscriptions for a Graph and is
// idempotent.
func TestRemoveGraph(t *testing.T) {
	graphA := client.ObjectKey{Namespace: "ns", Name: "a"}
	graphB := client.ObjectKey{Namespace: "ns", Name: "b"}
	gk := schema.GroupKind{Group: "apps", Kind: "Deployment"}

	sw, _, _ := newTestWatcher(t)

	// graphA tracks Deployment statically, graphB is dynamic.
	a := sw.ForGraph(graphA)
	a.Track(gk)
	a.Done(true)
	b := sw.ForGraph(graphB)
	b.TrackDynamic()
	b.Done(true)

	assert.Equal(t, 2, sw.SubscribedGraphs())

	sw.RemoveGraph(graphA)
	assert.Empty(t, sw.GraphsForGroupKind(gk))
	assert.ElementsMatch(t, []client.ObjectKey{graphB}, sw.DynamicGraphs())

	// Idempotent.
	sw.RemoveGraph(graphA)
	sw.RemoveGraph(client.ObjectKey{Namespace: "x", Name: "ghost"})
}

// --- CRD event routing -----------------------------------------------

// TestCRDAddSeedsHashAndEnqueuesAllGraphs pins the CRD-add path: the
// hash is seeded, AND every subscribed Graph (static + dynamic) is
// enqueued so previously-failing compiles get a retry.
func TestCRDAddSeedsHashAndEnqueuesAllGraphs(t *testing.T) {
	sw, gi, si := newTestWatcher(t)

	g1 := client.ObjectKey{Namespace: "ns", Name: "g1"}
	g2 := client.ObjectKey{Namespace: "ns", Name: "g2"}
	gkWidget := schema.GroupKind{Group: "example.com", Kind: "Widget"}
	gkOther := schema.GroupKind{Group: "other.com", Kind: "Gadget"}

	s1 := sw.ForGraph(g1)
	s1.Track(gkWidget)
	s1.Done(true)
	s2 := sw.ForGraph(g2)
	s2.Track(gkOther) // unrelated to the incoming CRD
	s2.Done(true)

	sw.onAdd(makeCRD("example.com", "Widget", "initial"))

	// Hash seeded.
	assert.NotEmpty(t, sw.SchemaHash(gkWidget))
	// Both Graphs enqueued: g1 because it subscribes to Widget; g2
	// because Add enqueues every Graph (we don't know which were
	// stuck waiting for this Kind to appear).
	enq := drainEvents(sw, 100*time.Millisecond)
	assert.ElementsMatch(t, []client.ObjectKey{g1, g2}, enq)
	// Schema invalidator received the GK.
	assert.ElementsMatch(t, []schema.GroupKind{gkWidget}, si.snapshot())
	// Graph invalidator received both keys.
	assert.ElementsMatch(t, []client.ObjectKey{g1, g2}, gi.snapshot())
}

// TestCRDUpdateRoutingTable runs the Update event through every shape
// of subscriber: static match, static non-match, dynamic. The "no-op
// update" case (same hash) and the "schema actually changed" case sit
// next to each other so dedup is visible.
func TestCRDUpdateRoutingTable(t *testing.T) {
	g1 := client.ObjectKey{Namespace: "ns", Name: "g1"} // tracks Widget
	g2 := client.ObjectKey{Namespace: "ns", Name: "g2"} // tracks Gadget
	g3 := client.ObjectKey{Namespace: "ns", Name: "g3"} // dynamic

	tests := []struct {
		name          string
		first         *apiextensionsv1.CustomResourceDefinition
		second        *apiextensionsv1.CustomResourceDefinition
		wantEnqueued  []client.ObjectKey
		wantInvalida  []schema.GroupKind
	}{
		{
			name:         "schema-content-change-enqueues-subscriber-plus-dynamic",
			first:        makeCRD("example.com", "Widget", "v0"),
			second:       makeCRD("example.com", "Widget", "v1"),
			wantEnqueued: []client.ObjectKey{g1, g3},
			wantInvalida: []schema.GroupKind{{Group: "example.com", Kind: "Widget"}},
		},
		{
			name:         "non-schema-update-deduped-no-enqueue",
			first:        makeCRD("example.com", "Widget", "v0"),
			second:       makeCRD("example.com", "Widget", "v0"), // identical
			wantEnqueued: nil,
			wantInvalida: nil,
		},
		{
			name:         "unrelated-gk-update-still-enqueues-dynamic",
			first:        makeCRD("third.io", "Thing", "v0"),
			second:       makeCRD("third.io", "Thing", "v1"),
			wantEnqueued: []client.ObjectKey{g3}, // only dynamic; g1/g2 don't track this
			wantInvalida: []schema.GroupKind{{Group: "third.io", Kind: "Thing"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sw, _, si := newTestWatcher(t)

			s1 := sw.ForGraph(g1)
			s1.Track(schema.GroupKind{Group: "example.com", Kind: "Widget"})
			s1.Done(true)
			s2 := sw.ForGraph(g2)
			s2.Track(schema.GroupKind{Group: "third.io", Kind: "Gadget"})
			s2.Done(true)
			s3 := sw.ForGraph(g3)
			s3.TrackDynamic()
			s3.Done(true)

			// Seed: first event populates the hash. We drain whatever
			// it produced (the Add-path enqueues everything).
			sw.onAdd(tc.first)
			drainEvents(sw, 50*time.Millisecond)
			si.calls = nil

			// Update: this is the case under test.
			sw.onUpdate(tc.first, tc.second)
			got := drainEvents(sw, 100*time.Millisecond)
			assert.ElementsMatch(t, tc.wantEnqueued, got)
			assert.ElementsMatch(t, tc.wantInvalida, si.snapshot())
		})
	}
}

// TestCRDDeleteEvictsHashAndEnqueuesSubscribers covers the delete
// event: the hash entry is removed (so a future Add re-seeds), and
// affected Graphs are enqueued (they'll likely fail to compile against
// a missing Kind, which is the intended signal).
func TestCRDDeleteEvictsHashAndEnqueuesSubscribers(t *testing.T) {
	sw, _, si := newTestWatcher(t)

	g1 := client.ObjectKey{Namespace: "ns", Name: "g1"}
	g3 := client.ObjectKey{Namespace: "ns", Name: "g3"}
	gk := schema.GroupKind{Group: "example.com", Kind: "Widget"}

	s1 := sw.ForGraph(g1)
	s1.Track(gk)
	s1.Done(true)
	s3 := sw.ForGraph(g3)
	s3.TrackDynamic()
	s3.Done(true)

	sw.onAdd(makeCRD("example.com", "Widget", "seed"))
	drainEvents(sw, 50*time.Millisecond)
	si.calls = nil

	sw.onDelete(makeCRD("example.com", "Widget", "seed"))

	assert.Empty(t, sw.SchemaHash(gk), "hash entry must be evicted on delete")
	got := drainEvents(sw, 100*time.Millisecond)
	assert.ElementsMatch(t, []client.ObjectKey{g1, g3}, got)
	assert.ElementsMatch(t, []schema.GroupKind{gk}, si.snapshot())
}

// TestEventBufferOverflowDropsRatherThanBlocks ensures the watcher
// doesn't deadlock the informer goroutine when the work queue runs
// dry. Excess events drop with a log; the channel is the bound.
func TestEventBufferOverflowDropsRatherThanBlocks(t *testing.T) {
	sw := New(logr.Discard(), Config{
		Graphs:      &fakeGraphInvalidator{},
		Schemas:     &fakeSchemaInvalidator{},
		EventBuffer: 2,
	})
	graphA := client.ObjectKey{Namespace: "ns", Name: "a"}
	s := sw.ForGraph(graphA)
	s.TrackDynamic()
	s.Done(true)

	// Fire several CRD events without draining; only the first 2 fit.
	for i := 0; i < 10; i++ {
		sw.onUpdate(
			makeCRD("example.com", "Widget", "v0"),
			makeCRD("example.com", "Widget", "v"+string(rune('a'+i))),
		)
	}
	assert.Equal(t, 2, len(sw.events), "buffer should clamp at capacity")
}

// TestEnqueueAfterCloseIsNoop confirms the shutdown guard: once Start
// returns, further enqueues are dropped silently rather than panicking
// on a closed channel.
func TestEnqueueAfterCloseIsNoop(t *testing.T) {
	sw, _, _ := newTestWatcher(t)
	sw.closed.Store(true)

	sw.enqueue(client.ObjectKey{Namespace: "ns", Name: "graph"})
	assert.Equal(t, 0, len(sw.events))
}

// --- hashCRDSchema ---------------------------------------------------

// TestHashCRDSchemaSelectivity pins the contract: irrelevant changes
// (annotations, labels, resourceVersion) produce the same hash;
// schema-content changes produce different hashes.
func TestHashCRDSchemaSelectivity(t *testing.T) {
	base := makeCRD("example.com", "Widget", "v0")

	tests := []struct {
		name      string
		mutate    func(*apiextensionsv1.CustomResourceDefinition)
		sameHash  bool
	}{
		{
			name:     "annotation-tweak-same-hash",
			mutate:   func(c *apiextensionsv1.CustomResourceDefinition) { c.Annotations = map[string]string{"x": "y"} },
			sameHash: true,
		},
		{
			name:     "resourceVersion-bump-same-hash",
			mutate:   func(c *apiextensionsv1.CustomResourceDefinition) { c.ResourceVersion = "999" },
			sameHash: true,
		},
		{
			name:     "schema-description-change-different-hash",
			mutate:   func(c *apiextensionsv1.CustomResourceDefinition) { c.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "v1" },
			sameHash: false,
		},
		{
			name:     "kind-rename-different-hash",
			mutate:   func(c *apiextensionsv1.CustomResourceDefinition) { c.Spec.Names.Kind = "Gizmo" },
			sameHash: false,
		},
		{
			name:     "served-flag-flip-different-hash",
			mutate:   func(c *apiextensionsv1.CustomResourceDefinition) { c.Spec.Versions[0].Served = false },
			sameHash: false,
		},
	}

	baseHash := hashCRDSchema(base)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			copy := base.DeepCopy()
			tc.mutate(copy)
			got := hashCRDSchema(copy)
			if tc.sameHash {
				assert.Equal(t, baseHash, got)
			} else {
				assert.NotEqual(t, baseHash, got)
			}
		})
	}
}

// TestNoopFallbacks covers the no-invalidator path: when Graphs or
// Schemas is nil, CRD events still route correctly without panic.
func TestNoopFallbacks(t *testing.T) {
	sw := New(logr.Discard(), Config{EventBuffer: 16})
	graphA := client.ObjectKey{Namespace: "ns", Name: "a"}
	s := sw.ForGraph(graphA)
	s.Track(schema.GroupKind{Group: "example.com", Kind: "Widget"})
	s.Done(true)

	sw.onUpdate(
		makeCRD("example.com", "Widget", "v0"),
		makeCRD("example.com", "Widget", "v1"),
	)
	got := drainEvents(sw, 100*time.Millisecond)
	assert.ElementsMatch(t, []client.ObjectKey{graphA}, got)
}

// TestRequiredDoneCount confirms SubscribedGraphs returns 0 after the
// last Graph is removed.
func TestRequiredDoneCount(t *testing.T) {
	sw, _, _ := newTestWatcher(t)
	require.Equal(t, 0, sw.SubscribedGraphs())

	graphA := client.ObjectKey{Namespace: "ns", Name: "a"}
	s := sw.ForGraph(graphA)
	s.Track(schema.GroupKind{Group: "apps", Kind: "Deployment"})
	s.Done(true)
	require.Equal(t, 1, sw.SubscribedGraphs())

	sw.RemoveGraph(graphA)
	require.Equal(t, 0, sw.SubscribedGraphs())
}
