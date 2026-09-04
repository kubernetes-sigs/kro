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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/test/integration/environment"
)

var _ = Describe("Graph Compilation", func() {
	It("exercises the Accepted condition for valid and invalid Graph specs", func() {
		t := GinkgoT()
		tests := []struct {
			name       string
			nodes      []expv1alpha1.Node
			wantStatus metav1.ConditionStatus
			wantSubstr string // expected substring in the condition message (case-insensitive)
		}{
			{
				name: "valid-single-template",
				nodes: []expv1alpha1.Node{{
					ID: "cm",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "x"},
					}),
				}},
				wantStatus: metav1.ConditionTrue,
				wantSubstr: "compiled 1 nodes",
			},
			{
				name: "valid-dependency-chain",
				nodes: []expv1alpha1.Node{
					{
						ID:  "base",
						Def: environment.RawExt(t, map[string]any{"name": "alpha"}),
					},
					{
						ID: "cm",
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "${base.name}"},
						}),
					},
				},
				wantStatus: metav1.ConditionTrue,
				wantSubstr: "compiled 2 nodes",
			},
			{
				name: "invalid-cel-unknown-node",
				nodes: []expv1alpha1.Node{
					{
						ID:  "base",
						Def: environment.RawExt(t, map[string]any{"a": "x"}),
					},
					{
						ID: "cm",
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "${ghost.value}"},
						}),
					},
				},
				wantStatus: metav1.ConditionFalse,
				wantSubstr: "ghost",
			},
			{
				name: "invalid-cyclic-dependency",
				// Includes a literal input node so the cycle check (not the
				// input-node rule) is the failing pass. b and c reference
				// each other.
				nodes: []expv1alpha1.Node{
					{
						ID:  "seed",
						Def: environment.RawExt(t, map[string]any{"v": "x"}),
					},
					{
						ID: "b",
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "${c.metadata.name}"},
						}),
					},
					{
						ID: "c",
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "${b.metadata.name}"},
						}),
					},
				},
				wantStatus: metav1.ConditionFalse,
				wantSubstr: "cycle",
			},
			{
				name: "invalid-readywhen-non-bool",
				nodes: []expv1alpha1.Node{{
					ID: "cm",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "x"},
					}),
					ReadyWhen: []string{"${cm.metadata.name}"}, // string, not bool
				}},
				wantStatus: metav1.ConditionFalse,
				wantSubstr: "bool",
			},
			{
				name: "invalid-input-node-rule-missing",
				// A single node that references ITSELF, so there is no resolvable
				// root. The dependency-graph builder rejects the self-edge directly
				// ("self references are not allowed") — the graph is still refused
				// (Accepted=False), which is what this case guards. (Previously the
				// no-CEL-seed heuristic produced an "input node" message; that
				// heuristic was replaced by real cycle/resolvable-root validation.)
				nodes: []expv1alpha1.Node{
					{
						ID: "a",
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "${a.metadata.name}"},
						}),
					},
				},
				wantStatus: metav1.ConditionFalse,
				wantSubstr: "self references are not allowed",
			},
		}

		for _, tc := range tests {
			ns := env.CreateNamespace(t)
			g := &expv1alpha1.Graph{
				ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: ns},
				Spec:       expv1alpha1.GraphSpec{Nodes: tc.nodes},
			}
			env.CreateGraph(t, g)

			cond := env.AwaitCondition(t,
				types.NamespacedName{Namespace: ns, Name: "g"},
				expv1alpha1.GraphConditionTypeAccepted,
				tc.wantStatus,
				20*time.Second,
			)
			if tc.wantSubstr != "" {
				msg := ""
				if cond.Message != nil {
					msg = *cond.Message
				}
				if !strings.Contains(strings.ToLower(msg), strings.ToLower(tc.wantSubstr)) {
					t.Fatalf("[%s] condition message %q did not contain %q", tc.name, msg, tc.wantSubstr)
				}
			}
		}
	})
})
