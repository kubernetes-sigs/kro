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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	expv1alpha1 "sigs.k8s.io/krocodile/api/v1alpha1"
	"sigs.k8s.io/krocodile/test/integration/environment"
)

// TestDefTypeInferenceCatchesTypos exercises the def-schema inference
// path: the compiler synthesizes an OpenAPI schema from the def
// literal, so a misspelled field on a downstream CEL reference fails
// at compile time instead of at apply. Without inference this would
// only fail at runtime (or worse, silently produce a string-typed
// nil).
func TestDefTypeInferenceCatchesTypos(t *testing.T) {
	env := environment.Shared(t)

	tests := []struct {
		name       string
		nodes      []expv1alpha1.Node
		wantStatus metav1.ConditionStatus
		wantSubstr string
	}{
		{
			name: "valid-deep-field-reference-compiles",
			nodes: []expv1alpha1.Node{
				{
					ID: "naming",
					Def: environment.RawExt(t, map[string]any{
						"prefix": "team-",
						"app":    "billing",
					}),
				},
				{
					ID: "cm",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "${naming.prefix + naming.app}"},
					}),
				},
			},
			wantStatus: metav1.ConditionTrue,
		},
		{
			name: "typo-on-def-field-fails-at-compile",
			nodes: []expv1alpha1.Node{
				{
					ID: "naming",
					Def: environment.RawExt(t, map[string]any{
						"prefix": "team-",
						"app":    "billing",
					}),
				},
				{
					ID: "cm",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						// Typo: "ap" instead of "app". Inferred schema
						// has "app" only → CEL type check fails.
						"metadata": map[string]any{"name": "${naming.prefix + naming.ap}"},
					}),
				},
			},
			wantStatus: metav1.ConditionFalse,
			wantSubstr: "ap",
		},
		{
			name: "cel-fragment-keeps-field-dyn",
			nodes: []expv1alpha1.Node{
				{
					ID: "seed",
					Def: environment.RawExt(t, map[string]any{"v": "x"}),
				},
				{
					ID: "deferred",
					Def: environment.RawExt(t, map[string]any{
						// Containing ${ marks the field as dyn; downstream
						// references should compile.
						"name": "${'derived-' + seed.v}",
					}),
				},
				{
					ID: "cm",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "${deferred.name}"},
					}),
				},
			},
			wantStatus: metav1.ConditionTrue,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ns := env.CreateNamespace(t)
			g := &expv1alpha1.Graph{
				ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: ns},
				Spec:       expv1alpha1.GraphSpec{Nodes: tc.nodes},
			}
			env.CreateGraph(t, g)

			cond := env.AwaitCondition(t,
				types.NamespacedName{Namespace: ns, Name: "t"},
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
					t.Fatalf("condition message %q did not contain %q", msg, tc.wantSubstr)
				}
			}
		})
	}
}
