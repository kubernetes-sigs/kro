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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	expv1alpha1 "sigs.k8s.io/krocodile/api/v1alpha1"
	"sigs.k8s.io/krocodile/test/integration/environment"
)

// TestReadinessCases drives readyWhen via a table: each row defines a
// CEL expression and the expected Graph-level Ready status. The table
// is intentionally narrow — readiness is checked AFTER apply, so the
// behavior we lock down is "expression true → Ready, expression false
// → not Ready".
func TestReadinessCases(t *testing.T) {
	env := environment.Shared(t)

	tests := []struct {
		name      string
		readyExpr string
		want      metav1.ConditionStatus
	}{
		{
			name:      "satisfied-literal-data-equals-spec",
			readyExpr: `${cm.data.k == "v"}`,
			want:      metav1.ConditionTrue,
		},
		{
			name:      "unsatisfied-literal",
			readyExpr: `${cm.data.k == "other"}`,
			want:      metav1.ConditionFalse,
		},
		{
			name:      "satisfied-via-string-length-check",
			readyExpr: `${size(cm.data.k) > 0}`,
			want:      metav1.ConditionTrue,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ns := env.CreateNamespace(t)
			g := &expv1alpha1.Graph{
				ObjectMeta: metav1.ObjectMeta{Name: "rdy", Namespace: ns},
				Spec: expv1alpha1.GraphSpec{
					Nodes: []expv1alpha1.Node{{
						ID: "cm",
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "rdy-cm"},
							"data":       map[string]any{"k": "v"},
						}),
						ReadyWhen: []string{tc.readyExpr},
					}},
				},
			}
			env.CreateGraph(t, g)

			// Wait for the timed-requeue cycle (~1s) plus settle.
			env.AwaitCondition(t,
				types.NamespacedName{Namespace: ns, Name: "rdy"},
				expv1alpha1.GraphConditionTypeReady,
				tc.want, 15*time.Second)

			// In the unsatisfied case, the ConfigMap still exists —
			// readiness is a status concern, not an apply concern.
			env.AwaitObject(t, configMapGVK,
				types.NamespacedName{Namespace: ns, Name: "rdy-cm"},
				func(u *unstructured.Unstructured) error {
					if v, _, _ := unstructured.NestedString(u.Object, "data", "k"); v != "v" {
						return fmt.Errorf("data.k=%q want v", v)
					}
					return nil
				}, 10*time.Second)
		})
	}
}

// TestReadinessChainedDependency proves that an upstream node that is
// not ready blocks downstream readiness from being established —
// because the executor surfaces ErrWaitingForReadiness which the
// reconciler treats as ErrNotReady. The downstream node IS applied
// (apply runs before readyWhen in the loop), so we can't assert its
// absence — only that the Graph never reaches Ready=True.
func TestReadinessChainedDependency(t *testing.T) {
	env := environment.Shared(t)
	ns := env.CreateNamespace(t)

	g := &expv1alpha1.Graph{
		ObjectMeta: metav1.ObjectMeta{Name: "chain", Namespace: ns},
		Spec: expv1alpha1.GraphSpec{
			Nodes: []expv1alpha1.Node{
				{
					ID: "a",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "a"},
						"data":       map[string]any{"k": "1"},
					}),
					// Never satisfied — depends on the literal not matching.
					ReadyWhen: []string{`${a.data.k == "never"}`},
				},
				{
					ID: "b",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "b"},
						"data":       map[string]any{"upstream": "${a.metadata.name}"},
					}),
				},
			},
		},
	}
	env.CreateGraph(t, g)

	// Accepted should still flip True (the graph compiled).
	env.AwaitCondition(t,
		types.NamespacedName{Namespace: ns, Name: "chain"},
		expv1alpha1.GraphConditionTypeAccepted,
		metav1.ConditionTrue, 15*time.Second)

	// Ready must NOT reach True within a settling window — the apply
	// loop trips on a's readyWhen and surfaces ErrNotReady.
	environment.Consistently(t, 3*time.Second, 250*time.Millisecond, func() error {
		obj := &expv1alpha1.Graph{}
		if err := env.Client.Get(env.Ctx, types.NamespacedName{Namespace: ns, Name: "chain"}, obj); err != nil {
			return err
		}
		for _, c := range obj.Status.Conditions {
			if c.Type == expv1alpha1.GraphConditionTypeReady && c.Status == metav1.ConditionTrue {
				return fmt.Errorf("Ready unexpectedly True")
			}
		}
		return nil
	})
}
