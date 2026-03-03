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

	"github.com/stretchr/testify/require"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

func parserTestRaw(raw string) k8sruntime.RawExtension {
	return k8sruntime.RawExtension{Raw: []byte(raw)}
}

func parserTestTemplate(raw string) k8sruntime.RawExtension {
	return parserTestRaw(raw)
}

func TestParser_parseInstance_Cases(t *testing.T) {
	tests := []struct {
		name    string
		schema  *v1alpha1.Schema
		wantErr string
		assert  func(*testing.T, *ParsedInstance)
	}{
		{
			name: "invalid spec YAML",
			schema: &v1alpha1.Schema{
				Spec: parserTestRaw(`[1]`),
			},
			wantErr: "unmarshal spec",
		},
		{
			name: "invalid types YAML",
			schema: &v1alpha1.Schema{
				Spec:  parserTestRaw(`{"name":"string"}`),
				Types: parserTestRaw(`[1]`),
			},
			wantErr: "unmarshal types",
		},
		{
			name: "invalid status YAML",
			schema: &v1alpha1.Schema{
				Spec:   parserTestRaw(`{"name":"string"}`),
				Status: parserTestRaw(`[1]`),
			},
			wantErr: "unmarshal status",
		},
		{
			name: "status expression parsing failure",
			schema: &v1alpha1.Schema{
				Spec:   parserTestRaw(`{"name":"string"}`),
				Status: parserTestRaw(`{"value":"${outer(${inner})}"}`),
			},
			wantErr: "status expressions",
		},
		{
			name: "plain status fields are rejected",
			schema: &v1alpha1.Schema{
				Spec:   parserTestRaw(`{"name":"string"}`),
				Status: parserTestRaw(`{"phase":"Running"}`),
			},
			wantErr: "status fields without expressions are not supported",
		},
		{
			name: "success with metadata and printer columns",
			schema: &v1alpha1.Schema{
				Group:      "demo.kro.run",
				APIVersion: "v1alpha1",
				Kind:       "Demo",
				Spec:       parserTestRaw(`{"name":"string"}`),
				Types:      parserTestRaw(`{}`),
				Status:     parserTestRaw(`{"observed":"${svc.status.value}"}`),
				AdditionalPrinterColumns: []extv1.CustomResourceColumnDefinition{
					{Name: "Observed", Type: "string", JSONPath: ".status.observed"},
				},
				Metadata: &v1alpha1.CRDMetadata{
					Labels: map[string]string{"team": "graph"},
				},
			},
			assert: func(t *testing.T, got *ParsedInstance) {
				t.Helper()
				require.Equal(t, "demo.kro.run", got.Group)
				require.Equal(t, "v1alpha1", got.APIVersion)
				require.Equal(t, "Demo", got.Kind)
				require.NotNil(t, got.SpecSchema)
				require.Equal(t, map[string]interface{}{}, got.CustomTypes)
				require.Equal(t, map[string]interface{}{"observed": "${svc.status.value}"}, got.StatusTemplate)
				require.Len(t, got.PrinterColumns, 1)
				require.NotNil(t, got.Metadata)
				require.Equal(t, "graph", got.Metadata.Labels["team"])
				require.Len(t, got.StatusFields, 1)
				require.Equal(t, "observed", got.StatusFields[0].Path)
				require.True(t, got.StatusFields[0].Standalone)
				require.Len(t, got.StatusFields[0].Exprs, 1)
				require.Equal(t, "svc.status.value", got.StatusFields[0].Exprs[0].Raw)
			},
		},
	}

	p := &parser{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := p.parseInstance(tt.schema)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, got)
			if tt.assert != nil {
				tt.assert(t, got)
			}
		})
	}
}

func TestParser_parseNode_Cases(t *testing.T) {
	tests := []struct {
		name    string
		node    *v1alpha1.Resource
		index   int
		wantErr string
		assert  func(*testing.T, *ParsedNode)
	}{
		{
			name: "template unmarshal failure",
			node: &v1alpha1.Resource{
				ID:       "cfg",
				Template: parserTestTemplate(`[1]`),
			},
			wantErr: "unmarshal",
		},
		{
			name: "template expression extraction failure",
			node: &v1alpha1.Resource{
				ID: "cfg",
				Template: parserTestTemplate(`{
					"apiVersion":"v1",
					"kind":"ConfigMap",
					"metadata":{"name":"${outer(${inner})}"}
				}`),
			},
			wantErr: "extract expressions",
		},
		{
			name: "invalid readyWhen expression",
			node: &v1alpha1.Resource{
				ID:        "cfg",
				Template:  parserTestTemplate(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cfg"}}`),
				ReadyWhen: []string{"schema.spec.enabled"},
			},
			wantErr: "failed to parse readyWhen expressions",
		},
		{
			name: "invalid includeWhen expression",
			node: &v1alpha1.Resource{
				ID:          "cfg",
				Template:    parserTestTemplate(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cfg"}}`),
				IncludeWhen: []string{"schema.spec.enabled"},
			},
			wantErr: "failed to parse includeWhen expressions",
		},
		{
			name: "invalid forEach expression",
			node: &v1alpha1.Resource{
				ID:       "cfg",
				Template: parserTestTemplate(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cfg"}}`),
				ForEach: []v1alpha1.ForEachDimension{
					{"item": "schema.spec.items"},
				},
			},
			wantErr: "forEach",
		},
		{
			name: "scalar template resource",
			node: &v1alpha1.Resource{
				ID: "cfg",
				Template: parserTestTemplate(`{
					"apiVersion":"v1",
					"kind":"ConfigMap",
					"metadata":{"name":"cfg"},
					"data":{"value":"${schema.spec.name}"}
				}`),
			},
			assert: func(t *testing.T, got *ParsedNode) {
				t.Helper()
				require.Equal(t, "cfg", got.ID)
				require.Equal(t, 0, got.Index)
				require.Equal(t, NodeTypeScalar, got.Type)
				require.True(t, got.HasTemplate)
				require.False(t, got.HasExternalRef)
				require.Len(t, got.Fields, 1)
				require.Equal(t, "data.value", got.Fields[0].Path)
				require.True(t, got.Fields[0].Standalone)
				require.Equal(t, "schema.spec.name", got.Fields[0].Exprs[0].Raw)
			},
		},
		{
			name: "collection template resource",
			node: &v1alpha1.Resource{
				ID: "cfg",
				Template: parserTestTemplate(`{
					"apiVersion":"v1",
					"kind":"ConfigMap",
					"metadata":{"name":"${item}"}
				}`),
				ForEach: []v1alpha1.ForEachDimension{
					{"item": "${schema.spec.items}"},
				},
			},
			index: 3,
			assert: func(t *testing.T, got *ParsedNode) {
				t.Helper()
				require.Equal(t, NodeTypeCollection, got.Type)
				require.Equal(t, 3, got.Index)
				require.Len(t, got.ForEach, 1)
				require.Equal(t, "item", got.ForEach[0].Name)
				require.Equal(t, "schema.spec.items", got.ForEach[0].Expr.Raw)
			},
		},
		{
			name: "externalRef includes namespace in synthetic template",
			node: &v1alpha1.Resource{
				ID: "sharedCfg",
				ExternalRef: &v1alpha1.ExternalRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Metadata: v1alpha1.ExternalRefMetadata{
						Name:      "global-config",
						Namespace: "team-a",
					},
				},
			},
			assert: func(t *testing.T, got *ParsedNode) {
				t.Helper()
				require.Equal(t, NodeTypeExternal, got.Type)
				require.False(t, got.HasTemplate)
				require.True(t, got.HasExternalRef)
				require.NotNil(t, got.ExternalRef)

				md, ok := got.Template["metadata"].(map[string]interface{})
				require.True(t, ok)
				require.Equal(t, "global-config", md["name"])
				require.Equal(t, "team-a", md["namespace"])
			},
		},
	}

	p := &parser{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := p.parseNode(tt.node, tt.index)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, got)
			if tt.assert != nil {
				tt.assert(t, got)
			}
		})
	}
}

func TestExternalRefToTemplate_Cases(t *testing.T) {
	tests := []struct {
		name           string
		ref            *v1alpha1.ExternalRef
		wantHasNS      bool
		wantNamespace  string
		wantAPIVersion string
		wantKind       string
		wantName       string
	}{
		{
			name: "namespace omitted when empty",
			ref: &v1alpha1.ExternalRef{
				APIVersion: "v1",
				Kind:       "Secret",
				Metadata:   v1alpha1.ExternalRefMetadata{Name: "token"},
			},
			wantHasNS:      false,
			wantAPIVersion: "v1",
			wantKind:       "Secret",
			wantName:       "token",
		},
		{
			name: "namespace included when set",
			ref: &v1alpha1.ExternalRef{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Metadata: v1alpha1.ExternalRefMetadata{
					Name:      "backend",
					Namespace: "platform",
				},
			},
			wantHasNS:      true,
			wantNamespace:  "platform",
			wantAPIVersion: "apps/v1",
			wantKind:       "Deployment",
			wantName:       "backend",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := externalRefToTemplate(tt.ref)
			require.Equal(t, tt.wantAPIVersion, got["apiVersion"])
			require.Equal(t, tt.wantKind, got["kind"])

			md, ok := got["metadata"].(map[string]interface{})
			require.True(t, ok)
			require.Equal(t, tt.wantName, md["name"])

			ns, hasNS := md["namespace"]
			require.Equal(t, tt.wantHasNS, hasNS)
			if tt.wantHasNS {
				require.Equal(t, tt.wantNamespace, ns)
			}
		})
	}
}

func TestParseForEach_Cases(t *testing.T) {
	tests := []struct {
		name    string
		dims    []v1alpha1.ForEachDimension
		wantNil bool
		wantErr string
		assert  func(*testing.T, []*RawForEach)
	}{
		{
			name:    "no dimensions",
			dims:    nil,
			wantNil: true,
		},
		{
			name: "empty dimension map yields no iterators",
			dims: []v1alpha1.ForEachDimension{
				{},
			},
			assert: func(t *testing.T, got []*RawForEach) {
				t.Helper()
				require.Empty(t, got)
			},
		},
		{
			name: "non-standalone expression is rejected",
			dims: []v1alpha1.ForEachDimension{
				{"item": "schema.spec.items"},
			},
			wantErr: `dimension "item": only standalone expressions are allowed`,
		},
		{
			name: "nested expression is rejected",
			dims: []v1alpha1.ForEachDimension{
				{"item": "${outer(${inner})}"},
			},
			wantErr: `dimension "item"`,
		},
		{
			name: "valid dimensions",
			dims: []v1alpha1.ForEachDimension{
				{"item": "${schema.spec.items}"},
				{"zone": "${schema.spec.zones}"},
			},
			assert: func(t *testing.T, got []*RawForEach) {
				t.Helper()
				require.Len(t, got, 2)
				require.Equal(t, "item", got[0].Name)
				require.Equal(t, "schema.spec.items", got[0].Expr.Raw)
				require.Equal(t, "zone", got[1].Name)
				require.Equal(t, "schema.spec.zones", got[1].Expr.Raw)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseForEach(tt.dims)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			if tt.wantNil {
				require.Nil(t, got)
				return
			}
			if tt.assert != nil {
				tt.assert(t, got)
			}
		})
	}
}
