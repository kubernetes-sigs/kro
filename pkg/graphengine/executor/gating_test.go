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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

// cmExists reports whether a ConfigMap is present in the cluster.
func cmExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(configMapGVK)
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, cm)
	if err == nil {
		return true
	}
	require.True(t, apierrors.IsNotFound(err), "unexpected error fetching %q: %v", name, err)
	return false
}

// chainGraph builds a → b → c, where b reads a field of a and c reads a field
// of b, so the dependency edges are inferred from the CEL references. readyWhen
// on a is supplied by the caller so a row can make the head of the chain
// either ready or not.
func chainGraph(headReadyWhen string) *expv1alpha1.Graph {
	return generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithTemplate("a", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "a"},
			"data":     map[string]any{"k": "v"},
		}),
		generator.WithReadyWhen(headReadyWhen),
		generator.WithTemplate("b", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "b"},
			"data":     map[string]any{"from": "${a.data.k}"},
		}),
		generator.WithTemplate("c", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "c"},
			"data":     map[string]any{"from": "${b.data.from}"},
		}),
	)
}

// GateReadiness is what preserves classic RGD ordering on the shared engine: a
// dependent is not created until the resources it depends on report ready. With
// the gate off (the Graph default) every reachable node is applied regardless of
// upstream readiness, so drift watches register across a not-ready node.
func TestSimple_GateReadiness(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		gate           bool
		headReadyWhen  string
		wantApplied    []string
		wantAbsent     []string
		wantUnresolved []string
		wantNotReady   bool
	}{
		{
			name:           "gate on withholds dependents of an unready node and cascades",
			gate:           true,
			headReadyWhen:  `${a.data.k == "not-yet"}`,
			wantApplied:    []string{"a"},
			wantAbsent:     []string{"b", "c"},
			wantUnresolved: []string{"b", "c"},
			wantNotReady:   true,
		},
		{
			name:          "gate off applies dependents across an unready node",
			gate:          false,
			headReadyWhen: `${a.data.k == "not-yet"}`,
			wantApplied:   []string{"a", "b", "c"},
			wantNotReady:  true,
		},
		{
			name:          "gate on applies the whole chain once the head is ready",
			gate:          true,
			headReadyWhen: `${a.data.k == "v"}`,
			wantApplied:   []string{"a", "b", "c"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
			ex := NewSimple(cl)
			ex.GateReadiness = tc.gate

			res, err := ex.Apply(context.Background(),
				compileAndBuild(t, chainGraph(tc.headReadyWhen)), watchrouter.NoopWatcher{})

			if tc.wantNotReady {
				require.Error(t, err)
				assert.True(t, errors.Is(err, ErrNotReady),
					"an unsatisfied readyWhen must surface as ErrNotReady, got %v", err)
			} else {
				require.NoError(t, err)
			}

			for _, name := range tc.wantApplied {
				assert.True(t, cmExists(t, cl, name), "%q should have been applied", name)
			}
			for _, name := range tc.wantAbsent {
				assert.False(t, cmExists(t, cl, name),
					"%q must not be created while its dependency is unready", name)
			}
			for _, id := range tc.wantUnresolved {
				assert.Contains(t, res.Unresolved, id,
					"a withheld node must be reported Unresolved so prune is skipped")
			}
		})
	}
}

// A withheld node must never be reported as Applied, because the caller uses
// ApplyResult.Applied as the managed-resource inventory. Recording a resource
// that was never created would make it a prune candidate on the next cycle.
func TestSimple_GateReadinessDoesNotReportWithheldAsApplied(t *testing.T) {
	t.Parallel()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	ex := NewSimple(cl)
	ex.GateReadiness = true

	res, _ := ex.Apply(context.Background(),
		compileAndBuild(t, chainGraph(`${a.data.k == "not-yet"}`)), watchrouter.NoopWatcher{})

	for _, applied := range res.Applied {
		assert.NotEqual(t, "b", applied.NodeID, "withheld node b must not be in Applied")
		assert.NotEqual(t, "c", applied.NodeID, "withheld node c must not be in Applied")
	}
}

// ApplyWithLabeler is the only path the instance controller calls
// (controller_graph_engine.go). It composes a per-call labeler over the
// struct-level one and runs the walk on a copy of the executor so concurrent
// reconciles for different instances cannot race on LabelInjector.
func TestSimple_ApplyWithLabeler(t *testing.T) {
	t.Parallel()

	singleTemplate := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithTemplate("a", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "a"},
			"data":     map[string]any{"k": "v"},
		}),
	)

	label := func(k, v string) func(*unstructured.Unstructured) {
		return func(obj *unstructured.Unstructured) {
			labels := obj.GetLabels()
			if labels == nil {
				labels = map[string]string{}
			}
			labels[k] = v
			obj.SetLabels(labels)
		}
	}

	getLabels := func(t *testing.T, c client.Client) map[string]string {
		t.Helper()
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(configMapGVK)
		require.NoError(t, c.Get(context.Background(),
			types.NamespacedName{Namespace: "default", Name: "a"}, cm))
		return cm.GetLabels()
	}

	t.Run("struct-level and per-call labels are both stamped", func(t *testing.T) {
		t.Parallel()
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		ex := NewSimple(cl).WithLabelInjector(label("kro.run/owned", "true"))

		_, err := ex.ApplyWithLabeler(context.Background(),
			compileAndBuild(t, singleTemplate), watchrouter.NoopWatcher{},
			label("kro.run/instance-name", "demo"))
		require.NoError(t, err)

		labels := getLabels(t, cl)
		assert.Equal(t, "true", labels["kro.run/owned"], "struct-level injector must run")
		assert.Equal(t, "demo", labels["kro.run/instance-name"], "per-call labeler must run")
	})

	t.Run("a nil per-call labeler still applies struct-level labels", func(t *testing.T) {
		t.Parallel()
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		ex := NewSimple(cl).WithLabelInjector(label("kro.run/owned", "true"))

		_, err := ex.ApplyWithLabeler(context.Background(),
			compileAndBuild(t, singleTemplate), watchrouter.NoopWatcher{}, nil)
		require.NoError(t, err)

		assert.Equal(t, "true", getLabels(t, cl)["kro.run/owned"])
	})

	// The composed labeler must not leak onto the receiver: two instances of the
	// same GVR reconcile concurrently through one executor, so a per-call
	// labeler that stuck would stamp the wrong instance's labels.
	t.Run("the per-call labeler does not leak onto the receiver", func(t *testing.T) {
		t.Parallel()
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		ex := NewSimple(cl).WithLabelInjector(label("kro.run/owned", "true"))

		_, err := ex.ApplyWithLabeler(context.Background(),
			compileAndBuild(t, singleTemplate), watchrouter.NoopWatcher{},
			label("kro.run/instance-name", "demo"))
		require.NoError(t, err)

		probe := &unstructured.Unstructured{Object: map[string]any{}}
		ex.LabelInjector(probe)
		assert.Equal(t, map[string]string{"kro.run/owned": "true"}, probe.GetLabels(),
			"the receiver's injector must still stamp only its own labels")
	})

	// ApplyWithLabeler runs the walk on a copy of the executor. Every field
	// that changes behaviour has to survive that copy: dropping GateReadiness
	// would silently apply dependents across an unready dependency on the only
	// path the instance controller uses.
	t.Run("GateReadiness survives the per-call copy", func(t *testing.T) {
		t.Parallel()
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		ex := NewSimple(cl)
		ex.GateReadiness = true

		res, err := ex.ApplyWithLabeler(context.Background(),
			compileAndBuild(t, chainGraph(`${a.data.k == "not-yet"}`)),
			watchrouter.NoopWatcher{}, label("kro.run/instance-name", "demo"))

		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrNotReady))
		assert.False(t, cmExists(t, cl, "b"),
			"GateReadiness must be honoured through ApplyWithLabeler")
		assert.Contains(t, res.Unresolved, "b")
	})

	t.Run("ApplyConcurrency survives the per-call copy", func(t *testing.T) {
		t.Parallel()
		const count = 10
		const bound = 3
		base := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		cl := &concurrencyTrackingClient{
			Client:     base,
			patchDelay: 2 * time.Millisecond,
		}
		ex := NewSimple(cl)
		ex.ApplyConcurrency = bound

		res, err := ex.ApplyWithLabeler(context.Background(),
			compileAndBuild(t, largeCollectionGraph(count)),
			watchrouter.NoopWatcher{}, label("kro.run/instance-name", "demo"))

		require.NoError(t, err)
		assert.Len(t, res.Applied, count)
		maxObserved := cl.maxSeen.Load()
		assert.Greater(t, maxObserved, int32(1))
		assert.LessOrEqual(t, maxObserved, int32(bound))
	})
}
