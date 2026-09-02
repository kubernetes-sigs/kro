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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// getFailClient fails every Get with a non-NotFound error.
type getFailClient struct {
	client.Client
	err error
}

func (g *getFailClient) Get(
	context.Context, client.ObjectKey, client.Object, ...client.GetOption,
) error {
	return g.err
}

// listFailClient fails every List.
type listFailClient struct {
	client.Client
	err error
}

func (l *listFailClient) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return l.err
}

func namedRefGraph() *expv1alpha1.Graph {
	return generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithRef("vpc", &expv1alpha1.ExternalRef{
			APIVersion: "v1", Kind: "ConfigMap",
			Metadata: expv1alpha1.ExternalRefMetadata{Name: "existing", Namespace: "default"},
		}),
	)
}

func selectorRefGraph() *expv1alpha1.Graph {
	return generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithRef("pods", &expv1alpha1.ExternalRef{
			APIVersion: "v1", Kind: "ConfigMap",
			Metadata: expv1alpha1.ExternalRefMetadata{
				Namespace: "default",
				Selector:  runtime.RawExtension{Raw: []byte(`{"matchLabels":{"tier":"db"}}`)},
			},
		}),
	)
}

// A referenced object that is not in the cluster yet is a soft condition: it
// may be created later by something else. It must requeue rather than fail, and
// its identity is not kro's to prune.
func TestSimple_ApplyRef_MissingObjectIsSoftNotReady(t *testing.T) {
	t.Parallel()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	res, err := NewSimple(cl).Apply(context.Background(),
		compileAndBuild(t, namedRefGraph()), watchrouter.NoopWatcher{})

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotReady),
		"an absent external ref must requeue, not fail the reconcile, got %v", err)
	assert.Contains(t, err.Error(), "not found")
	assert.Contains(t, res.Unresolved, "vpc",
		"an unresolved ref must be reported so prune is withheld")
	assert.Empty(t, res.Applied,
		"a read-only ref is never a managed resource, resolved or not")
}

// A Get failure that is not NotFound is a real cluster problem and must abort
// rather than being misread as "the object isn't there yet".
func TestSimple_ApplyRef_HardGetErrorAborts(t *testing.T) {
	t.Parallel()
	base := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	cl := &getFailClient{Client: base, err: errors.New("etcd unavailable")}

	_, err := NewSimple(cl).Apply(context.Background(),
		compileAndBuild(t, namedRefGraph()), watchrouter.NoopWatcher{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "etcd unavailable")
	assert.False(t, errors.Is(err, ErrNotReady),
		"a transport failure must not be softened into a readiness wait")
}

// A List failure on an external collection is hard: an empty list would be
// indistinguishable from "the selector matched nothing", and publishing an
// empty collection into scope would silently shrink whatever depends on it.
func TestSimple_ApplyRefCollection_ListFailureIsHard(t *testing.T) {
	t.Parallel()
	base := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	cl := &listFailClient{Client: base, err: errors.New("etcd unavailable")}

	_, err := NewSimple(cl).Apply(context.Background(),
		compileAndBuild(t, selectorRefGraph()), watchrouter.NoopWatcher{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "list external collection")
	assert.False(t, errors.Is(err, ErrNotReady))
}

// A failure to declare the external-collection watch must NOT abort (issue
// #17): the collection is still listed and published, only drift detection is
// lost. Apply succeeds and the ref node is not marked unresolved.
func TestSimple_ApplyRefCollection_WatchFailureIsSoftFail(t *testing.T) {
	t.Parallel()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	res, err := NewSimple(cl).Apply(context.Background(),
		compileAndBuild(t, selectorRefGraph()),
		failWatcher{err: errors.New("informer unavailable")})

	require.NoError(t, err,
		"a collection watch registration failure must not abort the apply")
	assert.NotContains(t, res.Unresolved, "pods",
		"a lost external-collection drift watch must not mark the ref unresolved")
}

// A collection template registers ONE selector watch for the whole node rather
// than N scalar watches, because the coordinator keys state by NodeID and N
// scalar watches would collapse to the last item. The selector must include the
// instance-id label when it is present: without it, two instances of the same
// RGD share a node-id and their watches would cross-match each other's
// resources.
func TestSimple_WatchCollectionSelectorScopesToInstance(t *testing.T) {
	t.Parallel()

	collectionGraph := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithDef("src", map[string]any{"names": []any{"alpha", "beta"}}),
		generator.WithTemplate("cm", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "${'cm-' + n}"},
			"data":     map[string]any{"k": "v"},
		}, generator.ForEachDim("n", "${src.names}")),
	)

	t.Run("the selector carries node-id and instance-id", func(t *testing.T) {
		t.Parallel()
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		ex := NewSimple(cl).WithLabelInjector(func(obj *unstructured.Unstructured) {
			l := obj.GetLabels()
			if l == nil {
				l = map[string]string{}
			}
			l[metadata.InstanceIDLabel] = "uid-1"
			obj.SetLabels(l)
		})
		w := &recordingWatcher{}

		_, err := ex.Apply(context.Background(), compileAndBuild(t, collectionGraph), w)
		require.NoError(t, err)

		require.Len(t, w.reqs, 1,
			"a collection node declares one selector watch, not one per item")
		sel := w.reqs[0].Selector
		require.NotNil(t, sel)
		assert.True(t, sel.Matches(labels.Set{
			metadata.NodeIDLabel: "cm", metadata.InstanceIDLabel: "uid-1",
		}), "the node's own resources must match")
		assert.False(t, sel.Matches(labels.Set{
			metadata.NodeIDLabel: "cm", metadata.InstanceIDLabel: "uid-2",
		}), "another instance's resources must not match this watch")
	})

	t.Run("without an instance-id label the selector falls back to node-id", func(t *testing.T) {
		t.Parallel()
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		w := &recordingWatcher{}

		_, err := NewSimple(cl).Apply(context.Background(),
			compileAndBuild(t, collectionGraph), w)
		require.NoError(t, err)

		require.Len(t, w.reqs, 1)
		sel := w.reqs[0].Selector
		require.NotNil(t, sel)
		assert.True(t, sel.Matches(labels.Set{metadata.NodeIDLabel: "cm"}),
			"node-id alone must still match when no instance-id was stamped")
	})
}
