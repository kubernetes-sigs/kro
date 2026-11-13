// Copyright 2025 The Kubernetes Authors.
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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

// gatedGraph builds a → b where b has a real data edge to a and additionally
// gates on a's readiness via propagateWhen. headReadyWhen decides whether a is
// ready; c has no gate and depends on b so it inherits b's hold.
func gatedGraph(headReadyWhen, gate string) *expv1alpha1.Graph {
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
		generator.WithPropagateWhen(gate),
		generator.WithTemplate("c", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "c"},
			"data":     map[string]any{"from": "${b.data.from}"},
		}),
	)
}

// propagateWhen is the per-node opt-in to wait on a dependency's readiness on
// the scope-gated (Graph) path, where a data edge alone waits only for the
// dependency to be applied. A held node is Unresolved (resources preserved,
// never pruned) and its dependents are held with it.
func TestSimple_PropagateWhen(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		headReadyWhen  string
		gate           string
		wantApplied    []string
		wantAbsent     []string
		wantUnresolved []string
		wantNotReady   bool
	}{
		{
			name:           "<id>.ready() false holds the gated node and its dependents",
			headReadyWhen:  `${a.data.k == "not-yet"}`,
			gate:           `${a.ready()}`,
			wantApplied:    []string{"a"},
			wantAbsent:     []string{"b", "c"},
			wantUnresolved: []string{"b", "c"},
			wantNotReady:   true,
		},
		{
			name:          "<id>.ready() true applies the whole chain",
			headReadyWhen: `${a.data.k == "v"}`,
			gate:          `${a.ready()}`,
			wantApplied:   []string{"a", "b", "c"},
		},
		{
			name:           "arbitrary predicate on upstream data holds",
			headReadyWhen:  `${a.data.k == "v"}`,
			gate:           `${a.data.k == "something-else"}`,
			wantApplied:    []string{"a"},
			wantAbsent:     []string{"b", "c"},
			wantUnresolved: []string{"b", "c"},
			wantNotReady:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
			ex := NewSimple(cl) // GateReadiness off: the Graph default

			res, err := ex.Apply(context.Background(),
				compileAndBuild(t, gatedGraph(tc.headReadyWhen, tc.gate)), watchrouter.NoopWatcher{})

			if tc.wantNotReady {
				require.Error(t, err)
				assert.True(t, errors.Is(err, ErrNotReady), "a held node must surface as ErrNotReady, got %v", err)
			} else {
				require.NoError(t, err)
			}
			for _, name := range tc.wantApplied {
				assert.True(t, cmExists(t, cl, name), "%q should have been applied", name)
			}
			for _, name := range tc.wantAbsent {
				assert.False(t, cmExists(t, cl, name), "%q must not be created while held", name)
			}
			for _, id := range tc.wantUnresolved {
				assert.Contains(t, res.Unresolved, id, "a held node must be Unresolved so prune is skipped")
			}
			for _, id := range tc.wantUnresolved {
				for _, mr := range res.Applied {
					assert.NotEqual(t, id, mr.NodeID, "a held node must never be reported Applied")
				}
			}
		})
	}
}

// A gate that reads a node not referenced anywhere else still orders the gated
// node after it: <id>.ready() creates a hard edge.
func TestSimple_PropagateWhenReadyRefCreatesEdge(t *testing.T) {
	t.Parallel()
	g := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithTemplate("gate", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "gate"},
			"data":     map[string]any{"ok": "false"},
		}),
		generator.WithReadyWhen(`${gate.data.ok == "true"}`),
		generator.WithTemplate("app", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "app"},
			"data":     map[string]any{"x": "y"}, // no data reference to gate
		}),
		generator.WithPropagateWhen(`${gate.ready()}`),
	)
	rt := compileAndBuild(t, g)
	var sawGate, sawApp bool
	for _, n := range rt.Nodes() {
		switch n.ID() {
		case "gate":
			sawGate = true
			assert.False(t, sawApp, "gate must be ordered before app")
		case "app":
			sawApp = true
			_, blocked := n.FirstUnreadyDep(map[string]bool{})
			assert.True(t, blocked, "app must have a hard dependency on gate via gate.ready()")
		}
	}
	require.True(t, sawGate && sawApp)

	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	_, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})
	require.Error(t, err)
	assert.True(t, cmExists(t, cl, "gate"))
	assert.False(t, cmExists(t, cl, "app"))
}
