// Copyright 2025 The Kube Resource Orchestrator Authors
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

package runtime

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	featuregatetesting "k8s.io/component-base/featuregate/testing"

	"github.com/kubernetes-sigs/kro/pkg/features"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
)

func TestNode_ConditionResults(t *testing.T) {
	// The same condition semantics serve RGDs and standalone Graphs. Run both
	// empty-optional representations serially because the gate is global.
	for _, omitEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("CELOmitFunction=%t", omitEnabled), func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.CELOmitFunction, omitEnabled)
			for _, tc := range []struct {
				name        string
				expr        string
				itemExpr    string
				immutable   any // nil means the field is absent
				wantFalse   bool
				wantPending bool
				wantErr     string
			}{
				{
					name: "empty optional", expr: "${cm.?immutable}", itemExpr: "${each.?immutable}",
					wantFalse: true,
				},
				{
					name: "optional false", expr: "${cm.?immutable}", itemExpr: "${each.?immutable}",
					immutable: false, wantFalse: true,
				},
				{
					name: "optional true", expr: "${cm.?immutable}", itemExpr: "${each.?immutable}",
					immutable: true,
				},
				{
					name: "dynamic null", expr: "${dyn(null)}", wantFalse: true,
				},
				{
					name: "dynamic nonboolean", expr: "${dyn('true')}", wantErr: "want bool",
				},
				{
					name: "dynamic optional nonboolean", expr: "${dyn(optional.of('true'))}", wantErr: "want bool",
				},
				{
					name: "optional evaluation error", expr: "${optional.of(1 / 0 == 0)}", wantErr: "division by zero",
				},
				{
					name: "conversion error", expr: "${dyn(b'true')}", wantErr: "bytes value cannot be used directly",
				},
				{
					name: "missing required field", expr: "${cm.immutable}", itemExpr: "${each.immutable}",
					wantPending: true,
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					itemExpr := tc.itemExpr
					if itemExpr == "" {
						itemExpr = tc.expr
					}
					g := generator.NewGraph("g",
						generator.WithTemplate("cm", map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "cm"},
						}),
						generator.WithReadyWhen(tc.expr),
						generator.WithTemplate("guarded", map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "guarded"},
						}),
						generator.WithIncludeWhen(tc.expr),
						generator.WithTemplate("cms", map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata":  map[string]any{"name": "${'cm-' + name}"},
							"immutable": true,
						}, generator.ForEachDim("name", "${['a', 'b']}")),
						generator.WithReadyWhen(itemExpr),
					)
					rt := New(compileGraph(t, g), g)
					cm, err := rt.Node("cm").Resolve()
					require.NoError(t, err)
					require.Len(t, cm, 1)
					if tc.immutable != nil {
						cm[0].Object["immutable"] = tc.immutable
					}
					rt.Set("cm", cm[0].Object)
					rt.Node("cm").SetObserved(cm, cm)
					items, err := rt.Node("cms").Resolve()
					require.NoError(t, err)
					require.Len(t, items, 2)
					// The first item is true: readiness must also examine the second.
					if tc.immutable == nil {
						delete(items[1].Object, "immutable")
					} else {
						items[1].Object["immutable"] = tc.immutable
					}
					rt.Node("cms").SetObserved(items, items)

					t.Run("includeWhen", func(t *testing.T) {
						ignored, err := rt.Node("guarded").IsIgnored()
						switch {
						case tc.wantPending:
							require.ErrorIs(t, err, ErrDataPending)
						case tc.wantErr != "":
							require.ErrorContains(t, err, tc.wantErr)
							assert.NotErrorIs(t, err, ErrDataPending)
						default:
							require.NoError(t, err)
						}
						assert.Equal(t, tc.wantFalse, ignored, "an error must not exclude a resource")
					})
					for _, id := range []string{"cm", "cms"} {
						t.Run("readyWhen/"+id, func(t *testing.T) {
							err := rt.Node(id).CheckReadiness()
							switch {
							case tc.wantPending || tc.wantFalse:
								require.ErrorIs(t, err, ErrWaitingForReadiness)
								if id == "cms" && tc.itemExpr != "" {
									assert.ErrorContains(t, err, "item 1")
								}
							case tc.wantErr != "":
								require.ErrorContains(t, err, tc.wantErr)
								assert.NotErrorIs(t, err, ErrWaitingForReadiness)
							default:
								require.NoError(t, err)
							}
						})
					}
					if tc.wantPending {
						// Pending inclusion must not cache a pruning-eligible verdict.
						cm[0].Object["immutable"] = true
						rt.Set("cm", cm[0].Object)
						items[1].Object["immutable"] = true
						ignored, err := rt.Node("guarded").IsIgnored()
						require.NoError(t, err)
						assert.False(t, ignored)
						require.NoError(t, rt.Node("cm").CheckReadiness())
						require.NoError(t, rt.Node("cms").CheckReadiness())
					}
				})
			}
		})
	}
}
