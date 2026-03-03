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
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/kubernetes-sigs/kro/pkg/graph/fieldpath"
)

func TestTypeChecker_Check_Cases(t *testing.T) {
	tests := []struct {
		name       string
		linkedRGD  func() *LinkedRGD
		wantErr    string
		assertFunc func(*testing.T, *TypeCheckResult)
	}{
		{
			name: "no nodes and no status fields yields empty status type map",
			linkedRGD: func() *LinkedRGD {
				return &LinkedRGD{
					Instance: minimalLinkedInstance(),
					Nodes:    nil,
				}
			},
			assertFunc: func(t *testing.T, result *TypeCheckResult) {
				require.NotNil(t, result)
				require.Empty(t, result.StatusTypes)
			},
		},
		{
			name: "collection readyWhen with schema succeeds",
			linkedRGD: func() *LinkedRGD {
				return linkedWithNodes(&LinkedNode{
					NodeIdentity: NodeIdentity{
						NodeMeta: NodeMeta{
							ID:   "pods",
							Type: NodeTypeCollection,
						},
						Schema: collectionStatusReadySchema(),
					},
					ReadyWhen: []*LinkedExpr{
						{Raw: "each.status.ready == true", References: []string{EachVarName}},
					},
				})
			},
			assertFunc: func(t *testing.T, result *TypeCheckResult) {
				require.NotNil(t, result)
				require.GreaterOrEqual(t, len(result.CheckedExprs), 1)
			},
		},
		{
			name: "collection readyWhen must return bool",
			linkedRGD: func() *LinkedRGD {
				return linkedWithNodes(&LinkedNode{
					NodeIdentity: NodeIdentity{
						NodeMeta: NodeMeta{
							ID:   "pods",
							Type: NodeTypeCollection,
						},
						Schema: collectionStatusReadySchema(),
					},
					ReadyWhen: []*LinkedExpr{
						{Raw: "1", References: nil},
					},
				})
			},
			wantErr: "must return bool",
		},
		{
			name: "collection readyWhen CEL parse failure returns error",
			linkedRGD: func() *LinkedRGD {
				return linkedWithNodes(&LinkedNode{
					NodeIdentity: NodeIdentity{
						NodeMeta: NodeMeta{
							ID:   "pods",
							Type: NodeTypeCollection,
						},
						Schema: collectionStatusReadySchema(),
					},
					ReadyWhen: []*LinkedExpr{
						{Raw: "each.status.ready ==", References: []string{EachVarName}},
					},
				})
			},
			wantErr: "readyWhen",
		},
		{
			name: "includeWhen must return bool",
			linkedRGD: func() *LinkedRGD {
				return linkedWithNodes(&LinkedNode{
					NodeIdentity: NodeIdentity{
						NodeMeta: NodeMeta{
							ID:   "cfg",
							Type: NodeTypeScalar,
						},
						Schema: metadataNameSchema(),
					},
					IncludeWhen: []*LinkedExpr{
						{Raw: "1", References: nil},
					},
				})
			},
			wantErr: "must return bool",
		},
		{
			name: "includeWhen CEL parse failure returns error",
			linkedRGD: func() *LinkedRGD {
				return linkedWithNodes(&LinkedNode{
					NodeIdentity: NodeIdentity{
						NodeMeta: NodeMeta{
							ID:   "cfg",
							Type: NodeTypeScalar,
						},
						Schema: metadataNameSchema(),
					},
					IncludeWhen: []*LinkedExpr{
						{Raw: "schema.spec.flag ==", References: []string{SchemaVarName}},
					},
				})
			},
			wantErr: "includeWhen",
		},
		{
			name: "template expression type match succeeds",
			linkedRGD: func() *LinkedRGD {
				return linkedWithNodes(&LinkedNode{
					NodeIdentity: NodeIdentity{
						NodeMeta: NodeMeta{
							ID:   "cfg",
							Type: NodeTypeScalar,
						},
						Schema: metadataNameSchema(),
					},
					Fields: []*LinkedField{
						{
							Path:       "metadata.name",
							Standalone: true,
							Kind:       FieldDynamic,
							Exprs: []*LinkedExpr{
								{Raw: "'my-app'", References: nil},
							},
						},
					},
				})
			},
			assertFunc: func(t *testing.T, result *TypeCheckResult) {
				require.NotNil(t, result)
				require.GreaterOrEqual(t, len(result.CheckedExprs), 1)
			},
		},
		{
			name: "template expression type mismatch fails",
			linkedRGD: func() *LinkedRGD {
				return linkedWithNodes(&LinkedNode{
					NodeIdentity: NodeIdentity{
						NodeMeta: NodeMeta{
							ID:   "cfg",
							Type: NodeTypeScalar,
						},
						Schema: metadataNameSchema(),
					},
					Fields: []*LinkedField{
						{
							Path:       "metadata.name",
							Standalone: true,
							Kind:       FieldDynamic,
							Exprs: []*LinkedExpr{
								{Raw: "1 + 1", References: nil},
							},
						},
					},
				})
			},
			wantErr: "type mismatch",
		},
		{
			name: "forEach expression must return list",
			linkedRGD: func() *LinkedRGD {
				return linkedWithNodes(&LinkedNode{
					NodeIdentity: NodeIdentity{
						NodeMeta: NodeMeta{
							ID:   "pods",
							Type: NodeTypeCollection,
						},
						Schema: metadataNameSchema(),
					},
					ForEach: []*LinkedForEach{
						{
							Name: "item",
							Expr: &LinkedExpr{Raw: "1", References: nil},
						},
					},
					Fields: []*LinkedField{
						{
							Path:       "metadata.name",
							Standalone: true,
							Kind:       FieldIteration,
							Exprs: []*LinkedExpr{
								{Raw: "item", References: []string{"item"}},
							},
						},
					},
				})
			},
			wantErr: "must return a list",
		},
		{
			name: "forEach expression CEL parse failure returns error",
			linkedRGD: func() *LinkedRGD {
				return linkedWithNodes(&LinkedNode{
					NodeIdentity: NodeIdentity{
						NodeMeta: NodeMeta{
							ID:   "pods",
							Type: NodeTypeCollection,
						},
						Schema: metadataNameSchema(),
					},
					ForEach: []*LinkedForEach{
						{
							Name: "item",
							Expr: &LinkedExpr{Raw: "schema.spec.items[", References: []string{SchemaVarName}},
						},
					},
				})
			},
			wantErr: "forEach",
		},
		{
			name: "forEach iterator type is available to template checking",
			linkedRGD: func() *LinkedRGD {
				return linkedWithNodes(&LinkedNode{
					NodeIdentity: NodeIdentity{
						NodeMeta: NodeMeta{
							ID:   "pods",
							Type: NodeTypeCollection,
						},
						Schema: metadataNameSchema(),
					},
					ForEach: []*LinkedForEach{
						{
							Name: "item",
							Expr: &LinkedExpr{Raw: "['a', 'b']", References: nil},
						},
					},
					Fields: []*LinkedField{
						{
							Path:       "metadata.name",
							Standalone: true,
							Kind:       FieldIteration,
							Exprs: []*LinkedExpr{
								{Raw: "item", References: []string{"item"}},
							},
						},
					},
				})
			},
			assertFunc: func(t *testing.T, result *TypeCheckResult) {
				require.NotNil(t, result.IteratorTypes["pods"]["item"])
				require.Equal(t, "string", result.IteratorTypes["pods"]["item"].String())
			},
		},
		{
			name: "status standalone expression parse failure returns error",
			linkedRGD: func() *LinkedRGD {
				inst := minimalLinkedInstance()
				inst.StatusFields = []*LinkedField{
					{
						Path:       "count",
						Standalone: true,
						Kind:       FieldDynamic,
						Exprs: []*LinkedExpr{
							{Raw: "1 +", References: nil},
						},
					},
				}
				return &LinkedRGD{Instance: inst}
			},
			wantErr: "status \"count\"",
		},
		{
			name: "status interpolation expression must be string",
			linkedRGD: func() *LinkedRGD {
				inst := minimalLinkedInstance()
				inst.StatusFields = []*LinkedField{
					{
						Path:       "message",
						Standalone: false,
						Kind:       FieldDynamic,
						Exprs: []*LinkedExpr{
							{Raw: "1 + 1", References: nil},
						},
					},
				}
				return &LinkedRGD{Instance: inst}
			},
			wantErr: "type mismatch",
		},
		{
			name: "status interpolation expression parse failure returns error",
			linkedRGD: func() *LinkedRGD {
				inst := minimalLinkedInstance()
				inst.StatusFields = []*LinkedField{
					{
						Path:       "message",
						Standalone: false,
						Kind:       FieldDynamic,
						Exprs: []*LinkedExpr{
							{Raw: "'ok' +", References: nil},
						},
					},
				}
				return &LinkedRGD{Instance: inst}
			},
			wantErr: "status \"message\"",
		},
		{
			name: "status interpolation expression can be string",
			linkedRGD: func() *LinkedRGD {
				inst := minimalLinkedInstance()
				inst.StatusFields = []*LinkedField{
					{
						Path:       "message",
						Standalone: false,
						Kind:       FieldDynamic,
						Exprs: []*LinkedExpr{
							{Raw: "'ok'", References: nil},
						},
					},
				}
				return &LinkedRGD{Instance: inst}
			},
			assertFunc: func(t *testing.T, result *TypeCheckResult) {
				require.Equal(t, cel.StringType, result.StatusTypes["message"])
			},
		},
		{
			name: "status standalone expression is inferred",
			linkedRGD: func() *LinkedRGD {
				inst := minimalLinkedInstance()
				inst.StatusFields = []*LinkedField{
					{
						Path:       "count",
						Standalone: true,
						Kind:       FieldDynamic,
						Exprs: []*LinkedExpr{
							{Raw: "1 + 1", References: nil},
						},
					},
				}
				return &LinkedRGD{Instance: inst}
			},
			assertFunc: func(t *testing.T, result *TypeCheckResult) {
				require.Equal(t, cel.IntType, result.StatusTypes["count"])
			},
		},
	}

	tc := newTypeChecker()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := tc.Check(tt.linkedRGD())
			if tt.wantErr != "" {
				require.Error(t, err)
				require.True(t, IsTerminal(err))
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			if tt.assertFunc != nil {
				tt.assertFunc(t, result)
			}
		})
	}
}

func linkedWithNodes(nodes ...*LinkedNode) *LinkedRGD {
	return &LinkedRGD{
		Instance: minimalLinkedInstance(),
		Nodes:    nodes,
	}
}

func minimalLinkedInstance() *LinkedInstance {
	return &LinkedInstance{
		InstanceMeta: InstanceMeta{
			Group:      "graph.example.io",
			APIVersion: "v1alpha1",
			Kind:       "TestInstance",
			SpecSchema: &extv1.JSONSchemaProps{
				Type:       "object",
				Properties: map[string]extv1.JSONSchemaProps{},
			},
		},
	}
}

func metadataNameSchema() *spec.Schema {
	return &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"metadata": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"name": {
								SchemaProps: spec.SchemaProps{
									Type: []string{"string"},
								},
							},
						},
					},
				},
			},
		},
	}
}

func collectionStatusReadySchema() *spec.Schema {
	return &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"status": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"ready": {
								SchemaProps: spec.SchemaProps{
									Type: []string{"boolean"},
								},
							},
						},
					},
				},
			},
		},
	}
}

func TestExpectedType_Cases(t *testing.T) {
	root := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"metadata": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
						},
					},
				},
				"spec": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"ports": {
								SchemaProps: spec.SchemaProps{
									Type: []string{"array"},
									Items: &spec.SchemaOrArray{
										Schema: &spec.Schema{
											SchemaProps: spec.SchemaProps{
												Type: []string{"object"},
												Properties: map[string]spec.Schema{
													"port": {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	tests := []struct {
		name       string
		field      *LinkedField
		schema     *spec.Schema
		wantKind   cel.Kind
		wantDyn    bool
		resourceID string
	}{
		{
			name:       "non-standalone always string",
			field:      &LinkedField{Path: "metadata.name", Standalone: false},
			schema:     root,
			wantKind:   cel.StringKind,
			resourceID: "res",
		},
		{
			name:       "nil schema returns dyn",
			field:      &LinkedField{Path: "metadata.name", Standalone: true},
			schema:     nil,
			wantDyn:    true,
			resourceID: "res",
		},
		{
			name:       "invalid path returns dyn",
			field:      &LinkedField{Path: "spec.ports[", Standalone: true},
			schema:     root,
			wantDyn:    true,
			resourceID: "res",
		},
		{
			name:       "missing field returns dyn",
			field:      &LinkedField{Path: "spec.missing", Standalone: true},
			schema:     root,
			wantDyn:    true,
			resourceID: "res",
		},
		{
			name:       "valid scalar field returns string kind",
			field:      &LinkedField{Path: "metadata.name", Standalone: true},
			schema:     root,
			wantKind:   cel.StringKind,
			resourceID: "res",
		},
		{
			name:       "valid indexed field returns int kind",
			field:      &LinkedField{Path: "spec.ports[0].port", Standalone: true},
			schema:     root,
			wantKind:   cel.IntKind,
			resourceID: "res",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expectedType(tt.field, tt.schema, tt.resourceID)
			require.NotNil(t, got)
			if tt.wantDyn {
				require.Equal(t, cel.DynKind, got.Kind())
				return
			}
			require.Equal(t, tt.wantKind, got.Kind())
		})
	}
}

func TestLookupSchemaAtField_Cases(t *testing.T) {
	propSchema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
			},
		},
	}

	additionalWithSchema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			AdditionalProperties: &spec.SchemaOrBool{
				Allows: true,
				Schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
			},
		},
	}

	additionalAllows := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			AdditionalProperties: &spec.SchemaOrBool{
				Allows: true,
			},
		},
	}

	arraySchema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"array"},
			Items: &spec.SchemaOrArray{
				Schema: &spec.Schema{
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
						},
					},
				},
			},
		},
	}

	tests := []struct {
		name      string
		schema    *spec.Schema
		field     string
		wantNil   bool
		wantKind  cel.Kind
		expectAny bool
	}{
		{
			name:     "properties branch",
			schema:   propSchema,
			field:    "name",
			wantKind: cel.StringKind,
		},
		{
			name:     "additionalProperties schema branch",
			schema:   additionalWithSchema,
			field:    "anything",
			wantKind: cel.IntKind,
		},
		{
			name:      "additionalProperties allows branch returns empty schema",
			schema:    additionalAllows,
			field:     "anything",
			wantNil:   false,
			expectAny: true,
		},
		{
			name:     "items recursion branch",
			schema:   arraySchema,
			field:    "name",
			wantKind: cel.StringKind,
		},
		{
			name:    "not found returns nil",
			schema:  propSchema,
			field:   "missing",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := lookupSchemaAtField(tt.schema, tt.field)
			if tt.wantNil {
				require.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			if tt.expectAny {
				return
			}
			celType := celTypeFromSchema(*got, "__type_test")
			require.NotNil(t, celType)
			require.Equal(t, tt.wantKind, celType.Kind())
		})
	}
}

func TestResolveSchemaAndTypeName_Cases(t *testing.T) {
	root := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"metadata": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
						},
					},
				},
			},
		},
	}

	tests := []struct {
		name       string
		path       string
		wantErr    string
		wantType   spec.StringOrArray
		wantSuffix string
	}{
		{
			name:       "valid nested path resolves schema and type name",
			path:       "metadata.name",
			wantType:   spec.StringOrArray{"string"},
			wantSuffix: ".metadata.name",
		},
		{
			name:    "indexed access on non-array returns error",
			path:    "metadata[0]",
			wantErr: "field is not an array",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			segments, err := fieldpath.Parse(tt.path)
			require.NoError(t, err)

			gotSchema, gotTypeName, err := resolveSchemaAndTypeName(segments, root, "res")
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, gotSchema)
			require.Equal(t, tt.wantType, gotSchema.Type)
			require.Contains(t, gotTypeName, tt.wantSuffix)
		})
	}
}

func TestCelTypeFromSchema_Cases(t *testing.T) {
	tests := []struct {
		name     string
		schema   spec.Schema
		wantKind cel.Kind
	}{
		{
			name:     "empty schema returns dyn",
			schema:   spec.Schema{},
			wantKind: cel.DynKind,
		},
		{
			name: "string schema returns string type",
			schema: spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"string"},
				},
			},
			wantKind: cel.StringKind,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := celTypeFromSchema(tt.schema, "__type_demo")
			require.NotNil(t, got)
			require.Equal(t, tt.wantKind, got.Kind())
		})
	}
}

func TestInstanceSchemaWithoutStatusForCEL_Cases(t *testing.T) {
	tests := []struct {
		name          string
		specSchema    *extv1.JSONSchemaProps
		wantErr       string
		assertSuccess func(*testing.T, *spec.Schema)
	}{
		{
			name: "success preserves existing properties and injects metadata",
			specSchema: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"replicas": {Type: "integer"},
				},
			},
			assertSuccess: func(t *testing.T, s *spec.Schema) {
				require.NotNil(t, s)
				specRoot, hasSpec := s.Properties["spec"]
				require.True(t, hasSpec)
				_, hasReplicas := specRoot.Properties["replicas"]
				require.True(t, hasReplicas)
				_, hasAPIVersion := s.Properties["apiVersion"]
				require.True(t, hasAPIVersion)
				_, hasKind := s.Properties["kind"]
				require.True(t, hasKind)
				_, hasMetadata := s.Properties["metadata"]
				require.True(t, hasMetadata)
			},
		},
		{
			name: "success initializes properties when absent",
			specSchema: &extv1.JSONSchemaProps{
				Type: "object",
			},
			assertSuccess: func(t *testing.T, s *spec.Schema) {
				require.NotNil(t, s)
				require.NotNil(t, s.Properties)
				_, hasMetadata := s.Properties["metadata"]
				require.True(t, hasMetadata)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := instanceSchemaWithoutStatusForCEL(tt.specSchema)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			if tt.assertSuccess != nil {
				tt.assertSuccess(t, got)
			}
		})
	}
}
