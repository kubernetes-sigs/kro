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

package core_test

import (
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	expv1alpha1 "sigs.k8s.io/krocodile/api/v1alpha1"
	"sigs.k8s.io/krocodile/test/integration/environment"
)

// TestContagiousIgnore drives the contract that includeWhen=false on
// one node propagates to its downstream dependents: a child of an
// ignored node is itself ignored, even if its own includeWhen
// evaluates true. Cases are layered to prove transitivity end-to-end.
func TestContagiousIgnore(t *testing.T) {
	env := environment.Shared(t)

	tests := []struct {
		name     string
		flagOn   bool
		wantA    bool // expect node-a ConfigMap to exist
		wantB    bool // expect node-b ConfigMap (depends on a)
		wantC    bool // expect node-c ConfigMap (depends on b)
		wantSibD bool // expect node-d (no chain dep) ConfigMap
	}{
		{
			name:     "flag=true keeps all nodes active",
			flagOn:   true,
			wantA:    true,
			wantB:    true,
			wantC:    true,
			wantSibD: true,
		},
		{
			name:     "flag=false ignores a, b, c contagiously; sibling-d stays",
			flagOn:   false,
			wantA:    false,
			wantB:    false,
			wantC:    false,
			wantSibD: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ns := env.CreateNamespace(t)
			g := &expv1alpha1.Graph{
				ObjectMeta: metav1.ObjectMeta{Name: "chain", Namespace: ns},
				Spec: expv1alpha1.GraphSpec{
					Nodes: []expv1alpha1.Node{
						{
							ID:  "flag",
							Def: environment.RawExt(t, map[string]any{"on": tc.flagOn}),
						},
						{
							ID:          "a",
							IncludeWhen: []string{"${flag.on}"},
							Template: environment.RawExt(t, map[string]any{
								"apiVersion": "v1",
								"kind":       "ConfigMap",
								"metadata":   map[string]any{"name": "a"},
								"data":       map[string]any{"k": "a"},
							}),
						},
						{
							ID: "b",
							// Depends on a via CEL — if a is ignored, b is too.
							Template: environment.RawExt(t, map[string]any{
								"apiVersion": "v1",
								"kind":       "ConfigMap",
								"metadata":   map[string]any{"name": "b"},
								"data":       map[string]any{"upstream": "${a.metadata.name}"},
							}),
						},
						{
							ID: "c",
							Template: environment.RawExt(t, map[string]any{
								"apiVersion": "v1",
								"kind":       "ConfigMap",
								"metadata":   map[string]any{"name": "c"},
								"data":       map[string]any{"upstream": "${b.metadata.name}"},
							}),
						},
						{
							ID: "d",
							// No dependency on the chain → unaffected.
							Template: environment.RawExt(t, map[string]any{
								"apiVersion": "v1",
								"kind":       "ConfigMap",
								"metadata":   map[string]any{"name": "d"},
							}),
						},
					},
				},
			}
			env.CreateGraph(t, g)

			env.AwaitCondition(t,
				types.NamespacedName{Namespace: ns, Name: "chain"},
				expv1alpha1.GraphConditionTypeAccepted,
				metav1.ConditionTrue, 15*time.Second)

			checks := []struct {
				name string
				want bool
			}{
				{"a", tc.wantA},
				{"b", tc.wantB},
				{"c", tc.wantC},
				{"d", tc.wantSibD},
			}
			for _, c := range checks {
				assertConfigMapPresence(t, env, ns, c.name, c.want)
			}
		})
	}
}

// assertConfigMapPresence either waits for the named CM to exist or
// asserts it stays absent for 1.5s — enough to flush a stray timer
// requeue without making the suite slow.
func assertConfigMapPresence(t *testing.T, env *environment.Env, ns, name string, wantExist bool) {
	t.Helper()
	key := types.NamespacedName{Namespace: ns, Name: name}
	if wantExist {
		env.AwaitObject(t, configMapGVK, key, nil, 10*time.Second)
		return
	}
	environment.Consistently(t, 1500*time.Millisecond, 100*time.Millisecond, func() error {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(configMapGVK)
		err := env.Client.Get(env.Ctx, key, obj)
		if err == nil {
			return fmt.Errorf("ConfigMap %s should not exist", name)
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("unexpected error fetching %s: %w", name, err)
		}
		return nil
	})
}
