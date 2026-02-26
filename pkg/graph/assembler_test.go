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

package graph

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/require"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph/dag"
)

func TestAssembler_Assemble_Cases(t *testing.T) {
	tests := []struct {
		name    string
		input   *CompiledRGD
		wantErr string
		assert  func(*testing.T, *Graph)
	}{
		{
			name: "status schema generation failure is returned",
			input: &CompiledRGD{
				Instance: &CompiledInstance{
					StatusTypes: map[string]*cel.Type{
						"bad.path": nil,
					},
				},
			},
			wantErr: "assembler: status schema",
		},
		{
			name: "fills defaults and carries metadata/printer columns",
			input: &CompiledRGD{
				Instance: &CompiledInstance{
					InstanceMeta: InstanceMeta{
						Group:      "demo.kro.run",
						APIVersion: "v1alpha1",
						Kind:       "Demo",
						SpecSchema: &extv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]extv1.JSONSchemaProps{
								"spec": {Type: "object"},
							},
						},
						StatusTemplate: map[string]interface{}{
							"message": "${cfg.status.value}",
						},
						PrinterColumns: []extv1.CustomResourceColumnDefinition{
							{Name: "Ready", Type: "string", JSONPath: ".status.ready"},
						},
						Metadata: &v1alpha1.CRDMetadata{
							Labels: map[string]string{
								"team": "graph",
							},
							Annotations: map[string]string{
								"owner": "kro",
							},
						},
					},
					StatusTypes: nil,
					StatusFields: []*CompiledVariable{
						{
							Path:       "message",
							Standalone: true,
							Kind:       FieldDynamic,
							Exprs: []*CompiledExpr{
								{Raw: "cfg.status.value", References: []string{"cfg"}},
							},
						},
						{
							Path:       "projection",
							Standalone: true,
							Kind:       FieldDynamic,
							Exprs: []*CompiledExpr{
								{Raw: "schema.spec.name", References: []string{SchemaVarName}},
							},
						},
					},
				},
				Nodes: []*CompiledNode{
					{
						Meta: CompiledNodeMeta{ID: "cfg"},
					},
				},
				DAG:              dag.NewDirectedAcyclicGraph[string](),
				TopologicalOrder: []string{"cfg"},
			},
			assert: func(t *testing.T, got *Graph) {
				t.Helper()
				require.NotNil(t, got)
				require.NotNil(t, got.CRD)
				require.Equal(t, []string{"cfg"}, got.TopologicalOrder)

				require.Equal(t, "graph", got.CRD.Labels["team"])
				require.Equal(t, "kro", got.CRD.Annotations["owner"])
				require.Len(t, got.CRD.Spec.Versions, 1)
				require.Len(t, got.CRD.Spec.Versions[0].AdditionalPrinterColumns, 1)
				require.Equal(t, "Ready", got.CRD.Spec.Versions[0].AdditionalPrinterColumns[0].Name)

				statusSchema := got.CRD.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
				require.Equal(t, "object", statusSchema.Type)

				require.NotNil(t, got.Instance)
				require.Equal(t, SchemaVarName, got.Instance.Meta.ID)
				require.Equal(t, NodeTypeInstance, got.Instance.Meta.Type)
				require.Equal(t, []string{"cfg"}, got.Instance.Meta.Dependencies)
				require.Len(t, got.Instance.Variables, 2)
				require.Equal(t, "status.message", got.Instance.Variables[0].Path)
				require.Equal(t, "status.projection", got.Instance.Variables[1].Path)
				require.Equal(t, map[string]interface{}{"message": "${cfg.status.value}"}, got.Instance.Template["status"])
			},
		},
	}

	a := &assembler{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := a.Assemble(tt.input)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				require.True(t, IsTerminal(err))
				return
			}

			require.NoError(t, err)
			if tt.assert != nil {
				tt.assert(t, got)
			}
		})
	}
}
