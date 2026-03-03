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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"
)

func validTemplate() map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": "test"},
	}
}

func validInstance() *ParsedInstance {
	return &ParsedInstance{
		InstanceMeta: InstanceMeta{
			Kind:       "MyApp",
			APIVersion: "v1alpha1",
		},
	}
}

func scalarNode(id string) *ParsedNode {
	return &ParsedNode{
		NodeMeta:    NodeMeta{ID: id, Template: validTemplate()},
		HasTemplate: true,
	}
}

func TestValidateInstance(t *testing.T) {
	tests := []struct {
		name       string
		kind       string
		apiVersion string
		wantErr    bool
		errContain string
	}{
		{name: "valid UpperCamelCase kind", kind: "MyApp", apiVersion: "v1alpha1"},
		{name: "single-word kind", kind: "App", apiVersion: "v1"},
		{name: "lowercase kind rejected", kind: "myApp", apiVersion: "v1", wantErr: true, errContain: "UpperCamelCase"},
		{name: "snake_case kind rejected", kind: "My_App", apiVersion: "v1", wantErr: true, errContain: "UpperCamelCase"},
		{name: "hyphenated kind rejected", kind: "My-App", apiVersion: "v1", wantErr: true, errContain: "UpperCamelCase"},
		{name: "numeric start kind rejected", kind: "1App", apiVersion: "v1", wantErr: true, errContain: "UpperCamelCase"},
		{name: "empty kind rejected", kind: "", apiVersion: "v1", wantErr: true},
		{name: "v1 valid", kind: "App", apiVersion: "v1"},
		{name: "v2 valid", kind: "App", apiVersion: "v2"},
		{name: "v10 valid", kind: "App", apiVersion: "v10"},
		{name: "v1alpha1 valid", kind: "App", apiVersion: "v1alpha1"},
		{name: "v1alpha2 valid", kind: "App", apiVersion: "v1alpha2"},
		{name: "v1alpha10 valid", kind: "App", apiVersion: "v1alpha10"},
		{name: "v15alpha1 valid", kind: "App", apiVersion: "v15alpha1"},
		{name: "v1beta1 valid", kind: "App", apiVersion: "v1beta1"},
		{name: "v1beta2 valid", kind: "App", apiVersion: "v1beta2"},
		{name: "bare v rejected", kind: "App", apiVersion: "v", wantErr: true, errContain: "not valid"},
		{name: "vvvv rejected", kind: "App", apiVersion: "vvvv", wantErr: true},
		{name: "dotted rejected", kind: "App", apiVersion: "v1.1", wantErr: true},
		{name: "double dotted rejected", kind: "App", apiVersion: "v1.1.1", wantErr: true},
		{name: "v1alpha (no digit) rejected", kind: "App", apiVersion: "v1alpha", wantErr: true},
		{name: "valpha1 rejected", kind: "App", apiVersion: "valpha1", wantErr: true},
		{name: "alpha without v rejected", kind: "App", apiVersion: "alpha", wantErr: true},
		{name: "1alpha without v rejected", kind: "App", apiVersion: "1alpha", wantErr: true},
		{name: "v1alpha1beta1 rejected", kind: "App", apiVersion: "v1alpha1beta1", wantErr: true},
		{name: "empty apiVersion rejected", kind: "App", apiVersion: "", wantErr: true},
	}

	v := &validator{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.validateInstance(&ParsedInstance{
				InstanceMeta: InstanceMeta{Kind: tt.kind, APIVersion: tt.apiVersion},
			})
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContain != "" {
					require.Contains(t, err.Error(), tt.errContain)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateNodeID(t *testing.T) {
	tests := []struct {
		name       string
		id         string
		wantErr    bool
		errContain string
	}{
		{name: "simple lowerCamel", id: "myNode"},
		{name: "single char", id: "x"},
		{name: "with digits", id: "node123"},
		{name: "camel with digits", id: "myNode42V2"},

		{name: "UpperCamelCase rejected", id: "MyNode", wantErr: true, errContain: "lowerCamelCase"},
		{name: "snake_case rejected", id: "my_node", wantErr: true, errContain: "lowerCamelCase"},
		{name: "hyphenated rejected", id: "my-node", wantErr: true, errContain: "lowerCamelCase"},
		{name: "starts with digit rejected", id: "1node", wantErr: true, errContain: "lowerCamelCase"},
		{name: "empty rejected", id: "", wantErr: true, errContain: "lowerCamelCase"},
		{name: "contains space rejected", id: "my node", wantErr: true, errContain: "lowerCamelCase"},
		{name: "contains dot rejected", id: "my.node", wantErr: true, errContain: "lowerCamelCase"},

		{name: "CEL reserved: true", id: "true", wantErr: true, errContain: "reserved"},
		{name: "CEL reserved: false", id: "false", wantErr: true, errContain: "reserved"},
		{name: "CEL reserved: null", id: "null", wantErr: true, errContain: "reserved"},
		{name: "CEL reserved: in", id: "in", wantErr: true, errContain: "reserved"},
		{name: "CEL reserved: break", id: "break", wantErr: true, errContain: "reserved"},

		{name: "kro reserved: schema", id: "schema", wantErr: true, errContain: "reserved"},
		{name: "kro reserved: instance", id: "instance", wantErr: true, errContain: "reserved"},
		{name: "kro reserved: each", id: "each", wantErr: true, errContain: "reserved"},
		{name: "kro reserved: spec", id: "spec", wantErr: true, errContain: "reserved"},
		{name: "kro reserved: status", id: "status", wantErr: true, errContain: "reserved"},
		{name: "kro reserved: metadata", id: "metadata", wantErr: true, errContain: "reserved"},
		{name: "kro reserved: namespace", id: "namespace", wantErr: true, errContain: "reserved"},
		{name: "kro reserved: kind", id: "kind", wantErr: true, errContain: "reserved"},
		{name: "kro reserved: resources", id: "resources", wantErr: true, errContain: "reserved"},
		{name: "kro reserved: self", id: "self", wantErr: true, errContain: "reserved"},
		{name: "kro reserved: resourceGraphDefinition", id: "resourceGraphDefinition", wantErr: true, errContain: "reserved"},
		{name: "kro reserved: resourcegraphdefinition", id: "resourcegraphdefinition", wantErr: true, errContain: "reserved"},

		{name: "all-caps not lowerCamelCase", id: "INSTANCE", wantErr: true, errContain: "lowerCamelCase"},
	}

	v := &validator{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.validateNodeID(tt.id, map[string]struct{}{})
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContain != "" {
					require.Contains(t, err.Error(), tt.errContain)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateNodeID_Duplicates_Cases(t *testing.T) {
	tests := []struct {
		name       string
		id         string
		seen       map[string]struct{}
		wantErr    bool
		errContain string
	}{
		{
			name: "new node id is accepted",
			id:   "myNode",
			seen: map[string]struct{}{},
		},
		{
			name: "duplicate node id is rejected",
			id:   "myNode",
			seen: map[string]struct{}{
				"myNode": {},
			},
			wantErr:    true,
			errContain: "duplicate",
		},
	}

	v := &validator{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.validateNodeID(tt.id, tt.seen)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContain != "" {
					require.Contains(t, err.Error(), tt.errContain)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateObjectStructure(t *testing.T) {
	tests := []struct {
		name       string
		obj        map[string]interface{}
		wantErr    bool
		errContain string
	}{
		{
			name:    "valid object",
			obj:     validTemplate(),
			wantErr: false,
		},
		{
			name: "missing apiVersion",
			obj: map[string]interface{}{
				"kind":     "Pod",
				"metadata": map[string]interface{}{},
			},
			wantErr:    true,
			errContain: "missing apiVersion",
		},
		{
			name: "apiVersion not string",
			obj: map[string]interface{}{
				"apiVersion": 123,
				"kind":       "Pod",
				"metadata":   map[string]interface{}{},
			},
			wantErr:    true,
			errContain: "apiVersion must be string",
		},
		{
			name: "missing kind",
			obj: map[string]interface{}{
				"apiVersion": "v1",
				"metadata":   map[string]interface{}{},
			},
			wantErr:    true,
			errContain: "missing kind",
		},
		{
			name: "kind not string",
			obj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       42,
				"metadata":   map[string]interface{}{},
			},
			wantErr:    true,
			errContain: "kind must be string",
		},
		{
			name: "missing metadata",
			obj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Pod",
			},
			wantErr:    true,
			errContain: "metadata",
		},
		{
			name: "metadata not a map",
			obj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata":   "not-a-map",
			},
			wantErr:    true,
			errContain: "metadata",
		},
		{
			name: "metadata is a list (not map)",
			obj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata":   []interface{}{"a", "b"},
			},
			wantErr:    true,
			errContain: "metadata",
		},
		{
			name: "extra fields are fine",
			obj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "foo"},
				"data":       map[string]interface{}{"key": "val"},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateObjectStructure(tt.obj)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContain != "" {
					require.Contains(t, err.Error(), tt.errContain)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateNoKROLabels(t *testing.T) {
	tests := []struct {
		name       string
		id         string
		obj        map[string]interface{}
		wantErr    bool
		errContain string
	}{
		{
			name: "no labels",
			id:   "res",
			obj: map[string]interface{}{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]interface{}{"name": "test"},
			},
		},
		{
			name: "empty labels",
			id:   "res",
			obj: map[string]interface{}{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]interface{}{"name": "test", "labels": map[string]interface{}{}},
			},
		},
		{
			name: "non-kro labels fine",
			id:   "res",
			obj: map[string]interface{}{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]interface{}{
					"name":   "test",
					"labels": map[string]interface{}{"app": "test", "team": "platform"},
				},
			},
		},
		{
			name: "kro.run/ prefixed label rejected",
			id:   "res",
			obj: map[string]interface{}{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]interface{}{
					"name":   "test",
					"labels": map[string]interface{}{"kro.run/owned": "true"},
				},
			},
			wantErr:    true,
			errContain: "kro.run/",
		},
		{
			name: "kro.run/ among other labels still rejected",
			id:   "res",
			obj: map[string]interface{}{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]interface{}{
					"name":   "test",
					"labels": map[string]interface{}{"app": "test", "kro.run/node-id": "x"},
				},
			},
			wantErr:    true,
			errContain: "reserved",
		},
		{
			name: "similar prefix without slash is fine",
			id:   "res",
			obj: map[string]interface{}{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]interface{}{
					"name":   "test",
					"labels": map[string]interface{}{"kro-run-owned": "false"},
				},
			},
		},
		{
			name: "metadata without labels field is fine",
			id:   "res",
			obj: map[string]interface{}{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]interface{}{"name": "test", "annotations": map[string]interface{}{}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNoKROLabels(tt.id, tt.obj)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContain != "" {
					require.Contains(t, err.Error(), tt.errContain)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateCRDExpressions(t *testing.T) {
	tests := []struct {
		name    string
		node    *ParsedNode
		wantErr bool
	}{
		{
			name: "non-CRD node with spec expressions is fine",
			node: &ParsedNode{
				NodeMeta: NodeMeta{
					ID:       "deployment",
					Template: map[string]interface{}{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]interface{}{}},
				},
				Fields: []*RawField{{Path: "spec.replicas", Exprs: []*RawExpr{{Raw: "schema.spec.replicas"}}}},
			},
		},
		{
			name: "CRD with metadata expression is fine",
			node: &ParsedNode{
				NodeMeta: NodeMeta{
					ID:       "crd",
					Template: map[string]interface{}{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]interface{}{}},
				},
				Fields: []*RawField{{Path: "metadata.name", Exprs: []*RawExpr{{Raw: "schema.spec.name"}}}},
			},
		},
		{
			name: "CRD with metadata.labels expression is fine",
			node: &ParsedNode{
				NodeMeta: NodeMeta{
					ID:       "crd",
					Template: map[string]interface{}{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]interface{}{}},
				},
				Fields: []*RawField{{Path: "metadata.labels.env", Exprs: []*RawExpr{{Raw: "schema.spec.env"}}}},
			},
		},
		{
			name: "CRD with spec expression rejected",
			node: &ParsedNode{
				NodeMeta: NodeMeta{
					ID:       "crd",
					Template: map[string]interface{}{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]interface{}{}},
				},
				Fields: []*RawField{{Path: "spec.group", Exprs: []*RawExpr{{Raw: "schema.spec.group"}}}},
			},
			wantErr: true,
		},
		{
			name: "CRD with status expression rejected",
			node: &ParsedNode{
				NodeMeta: NodeMeta{
					ID:       "crd",
					Template: map[string]interface{}{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]interface{}{}},
				},
				Fields: []*RawField{{Path: "status.conditions", Exprs: []*RawExpr{{Raw: "schema.spec.conditions"}}}},
			},
			wantErr: true,
		},
		{
			name: "CRD with no expressions is fine",
			node: &ParsedNode{
				NodeMeta: NodeMeta{
					ID:       "crd",
					Template: map[string]interface{}{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]interface{}{}},
				},
			},
		},
		{
			name: "v1 CRD spec subpath expression is still caught",
			node: &ParsedNode{
				NodeMeta: NodeMeta{
					ID:       "crd",
					Template: map[string]interface{}{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]interface{}{}},
				},
				Fields: []*RawField{{Path: "spec.names.kind", Exprs: []*RawExpr{{Raw: "schema.spec.kind"}}}},
			},
			wantErr: true,
		},
		{
			name: "apiextensions.k8s.io/v1beta1 is not caught (only v1)",
			node: &ParsedNode{
				NodeMeta: NodeMeta{
					ID:       "crd",
					Template: map[string]interface{}{"apiVersion": "apiextensions.k8s.io/v1beta1", "kind": "CustomResourceDefinition", "metadata": map[string]interface{}{}},
				},
				Fields: []*RawField{{Path: "spec.group", Exprs: []*RawExpr{{Raw: "schema.spec.group"}}}},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCRDExpressions(tt.node)
			if tt.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), "CEL expressions in CRDs")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateIterators(t *testing.T) {
	tests := []struct {
		name       string
		node       *ParsedNode
		otherIDs   []string // other node IDs in the graph
		wantErr    bool
		errContain string
	}{
		{
			name:     "no forEach is fine",
			node:     scalarNode("myNode"),
			otherIDs: nil,
		},
		{
			name: "valid iterator",
			node: &ParsedNode{
				NodeMeta:    NodeMeta{ID: "workers", Template: validTemplate()},
				HasTemplate: true,
				ForEach:     []*RawForEach{{Name: "worker", Expr: &RawExpr{Raw: "schema.spec.workers"}}},
			},
		},
		{
			name: "multiple valid iterators",
			node: &ParsedNode{
				NodeMeta:    NodeMeta{ID: "deployments", Template: validTemplate()},
				HasTemplate: true,
				ForEach: []*RawForEach{
					{Name: "region", Expr: &RawExpr{Raw: "schema.spec.regions"}},
					{Name: "tier", Expr: &RawExpr{Raw: "schema.spec.tiers"}},
				},
			},
		},
		{
			name: "iterator name not lowerCamelCase",
			node: &ParsedNode{
				NodeMeta:    NodeMeta{ID: "workers", Template: validTemplate()},
				HasTemplate: true,
				ForEach:     []*RawForEach{{Name: "Invalid_Name", Expr: &RawExpr{Raw: "schema.spec.workers"}}},
			},
			wantErr:    true,
			errContain: "lowerCamelCase",
		},
		{
			name: "iterator name is UpperCase",
			node: &ParsedNode{
				NodeMeta:    NodeMeta{ID: "workers", Template: validTemplate()},
				HasTemplate: true,
				ForEach:     []*RawForEach{{Name: "Worker", Expr: &RawExpr{Raw: "schema.spec.workers"}}},
			},
			wantErr:    true,
			errContain: "lowerCamelCase",
		},
		{
			name: "iterator name is reserved word: schema",
			node: &ParsedNode{
				NodeMeta:    NodeMeta{ID: "workers", Template: validTemplate()},
				HasTemplate: true,
				ForEach:     []*RawForEach{{Name: "schema", Expr: &RawExpr{Raw: "schema.spec.workers"}}},
			},
			wantErr:    true,
			errContain: "reserved",
		},
		{
			name: "iterator name is reserved word: each",
			node: &ParsedNode{
				NodeMeta:    NodeMeta{ID: "pods", Template: validTemplate()},
				HasTemplate: true,
				ForEach:     []*RawForEach{{Name: "each", Expr: &RawExpr{Raw: "schema.spec.podNames"}}},
			},
			wantErr:    true,
			errContain: "reserved",
		},
		{
			name: "iterator name is CEL reserved: true",
			node: &ParsedNode{
				NodeMeta:    NodeMeta{ID: "workers", Template: validTemplate()},
				HasTemplate: true,
				ForEach:     []*RawForEach{{Name: "true", Expr: &RawExpr{Raw: "schema.spec.workers"}}},
			},
			wantErr:    true,
			errContain: "reserved",
		},
		{
			name: "iterator name conflicts with resource ID",
			node: &ParsedNode{
				NodeMeta:    NodeMeta{ID: "backups", Template: validTemplate()},
				HasTemplate: true,
				ForEach:     []*RawForEach{{Name: "database", Expr: &RawExpr{Raw: "schema.spec.databases"}}},
			},
			otherIDs:   []string{"database"},
			wantErr:    true,
			errContain: "conflicts with resource ID",
		},
		{
			name: "duplicate iterator names in same node",
			node: &ParsedNode{
				NodeMeta:    NodeMeta{ID: "workers", Template: validTemplate()},
				HasTemplate: true,
				ForEach: []*RawForEach{
					{Name: "worker", Expr: &RawExpr{Raw: "schema.spec.workers"}},
					{Name: "worker", Expr: &RawExpr{Raw: "schema.spec.otherWorkers"}},
				},
			},
			wantErr:    true,
			errContain: "duplicate",
		},
	}

	v := &validator{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids := sets.NewString(tt.node.ID)
			for _, id := range tt.otherIDs {
				ids.Insert(id)
			}
			err := v.validateIterators(tt.node, ids)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContain != "" {
					require.Contains(t, err.Error(), tt.errContain)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateIterators_MaxCollectionDimensionSize(t *testing.T) {
	node := &ParsedNode{
		NodeMeta:    NodeMeta{ID: "deployments", Template: validTemplate()},
		HasTemplate: true,
		ForEach: []*RawForEach{
			{Name: "region", Expr: &RawExpr{Raw: "schema.spec.regions"}},
			{Name: "tier", Expr: &RawExpr{Raw: "schema.spec.tiers"}},
		},
	}

	ids := sets.NewString(node.ID)

	t.Run("default validator allows two dimensions", func(t *testing.T) {
		v := &validator{}
		require.NoError(t, v.validateIterators(node, ids))
	})

	t.Run("configured limit rejects too many dimensions", func(t *testing.T) {
		v, ok := newValidatorWithConfig(RGDConfig{MaxCollectionDimensionSize: 1}).(*validator)
		require.True(t, ok)

		err := v.validateIterators(node, ids)
		require.Error(t, err)
		require.Contains(t, err.Error(), "forEach cannot have more than 1 dimensions")
	})

	t.Run("configured zero limit rejects any dimension", func(t *testing.T) {
		v, ok := newValidatorWithConfig(RGDConfig{MaxCollectionDimensionSize: 0}).(*validator)
		require.True(t, ok)

		err := v.validateIterators(&ParsedNode{
			NodeMeta:    NodeMeta{ID: "workers", Template: validTemplate()},
			HasTemplate: true,
			ForEach:     []*RawForEach{{Name: "worker", Expr: &RawExpr{Raw: "schema.spec.workers"}}},
		}, sets.NewString("workers"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "forEach cannot have more than 0 dimensions")
	})
}

func TestValidateNode(t *testing.T) {
	tests := []struct {
		name       string
		node       *ParsedNode
		wantErr    bool
		errContain string
	}{
		{
			name:    "valid scalar node",
			node:    scalarNode("myNode"),
			wantErr: false,
		},
		{
			name: "neither template nor externalRef",
			node: &ParsedNode{
				NodeMeta: NodeMeta{ID: "broken"},
			},
			wantErr:    true,
			errContain: "must have template or externalRef",
		},
		{
			name: "both template and externalRef",
			node: &ParsedNode{
				NodeMeta:       NodeMeta{ID: "broken", Template: validTemplate()},
				HasTemplate:    true,
				HasExternalRef: true,
			},
			wantErr:    true,
			errContain: "cannot use externalRef with template",
		},
		{
			name: "externalRef with forEach rejected",
			node: &ParsedNode{
				NodeMeta:       NodeMeta{ID: "ext", Template: validTemplate()},
				HasExternalRef: true,
				ForEach:        []*RawForEach{{Name: "item", Expr: &RawExpr{Raw: "schema.spec.items"}}},
			},
			wantErr:    true,
			errContain: "cannot use externalRef with forEach",
		},
		{
			name: "template missing apiVersion",
			node: &ParsedNode{
				NodeMeta: NodeMeta{
					ID:       "bad",
					Template: map[string]interface{}{"kind": "Pod", "metadata": map[string]interface{}{}},
				},
				HasTemplate: true,
			},
			wantErr:    true,
			errContain: "apiVersion",
		},
		{
			name: "template missing kind",
			node: &ParsedNode{
				NodeMeta: NodeMeta{
					ID:       "bad",
					Template: map[string]interface{}{"apiVersion": "v1", "metadata": map[string]interface{}{}},
				},
				HasTemplate: true,
			},
			wantErr:    true,
			errContain: "kind",
		},
		{
			name: "template missing metadata",
			node: &ParsedNode{
				NodeMeta: NodeMeta{
					ID:       "bad",
					Template: map[string]interface{}{"apiVersion": "v1", "kind": "Pod"},
				},
				HasTemplate: true,
			},
			wantErr:    true,
			errContain: "metadata",
		},
		{
			name: "kro label in template rejected",
			node: &ParsedNode{
				NodeMeta: NodeMeta{
					ID: "labeled",
					Template: map[string]interface{}{
						"apiVersion": "v1", "kind": "ConfigMap",
						"metadata": map[string]interface{}{
							"name":   "test",
							"labels": map[string]interface{}{"kro.run/instance-id": "x"},
						},
					},
				},
				HasTemplate: true,
			},
			wantErr:    true,
			errContain: "reserved",
		},
	}

	v := &validator{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids := sets.NewString(tt.node.ID)
			err := v.validateNode(tt.node, ids)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContain != "" {
					require.Contains(t, err.Error(), tt.errContain)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidate_EndToEnd(t *testing.T) {
	tests := []struct {
		name       string
		parsed     *ParsedRGD
		wantErr    bool
		errContain string
	}{
		{
			name: "valid single-node RGD",
			parsed: &ParsedRGD{
				Instance: validInstance(),
				Nodes:    []*ParsedNode{scalarNode("configMap")},
			},
		},
		{
			name: "valid multi-node RGD",
			parsed: &ParsedRGD{
				Instance: validInstance(),
				Nodes: []*ParsedNode{
					scalarNode("deployment"),
					scalarNode("service"),
					scalarNode("ingress"),
				},
			},
		},
		{
			name: "invalid instance kind propagates",
			parsed: &ParsedRGD{
				Instance: &ParsedInstance{InstanceMeta: InstanceMeta{Kind: "badKind", APIVersion: "v1"}},
				Nodes:    []*ParsedNode{scalarNode("node")},
			},
			wantErr:    true,
			errContain: "UpperCamelCase",
		},
		{
			name: "duplicate node IDs",
			parsed: &ParsedRGD{
				Instance: validInstance(),
				Nodes: []*ParsedNode{
					scalarNode("myNode"),
					scalarNode("myNode"),
				},
			},
			wantErr:    true,
			errContain: "duplicate",
		},
		{
			name: "reserved node ID",
			parsed: &ParsedRGD{
				Instance: validInstance(),
				Nodes:    []*ParsedNode{scalarNode("schema")},
			},
			wantErr:    true,
			errContain: "reserved",
		},
		{
			name: "invalid node ID format",
			parsed: &ParsedRGD{
				Instance: validInstance(),
				Nodes: []*ParsedNode{
					{
						NodeMeta:    NodeMeta{ID: "my-node", Template: validTemplate()},
						HasTemplate: true,
					},
				},
			},
			wantErr:    true,
			errContain: "lowerCamelCase",
		},
		{
			name: "first invalid node fails fast",
			parsed: &ParsedRGD{
				Instance: validInstance(),
				Nodes: []*ParsedNode{
					scalarNode("goodNode"),
					{NodeMeta: NodeMeta{ID: "bad_node", Template: validTemplate()}, HasTemplate: true},
					scalarNode("anotherGood"),
				},
			},
			wantErr:    true,
			errContain: "lowerCamelCase",
		},
		{
			name: "node validation failure (missing template fields)",
			parsed: &ParsedRGD{
				Instance: validInstance(),
				Nodes: []*ParsedNode{
					{
						NodeMeta:    NodeMeta{ID: "broken", Template: map[string]interface{}{"kind": "Pod"}},
						HasTemplate: true,
					},
				},
			},
			wantErr:    true,
			errContain: "apiVersion",
		},
		{
			name: "errors are terminal",
			parsed: &ParsedRGD{
				Instance: validInstance(),
				Nodes:    []*ParsedNode{scalarNode("schema")},
			},
			wantErr: true,
		},
		{
			name: "forEach iterator conflicts with another node ID",
			parsed: &ParsedRGD{
				Instance: validInstance(),
				Nodes: []*ParsedNode{
					scalarNode("database"),
					{
						NodeMeta:    NodeMeta{ID: "backups", Template: validTemplate()},
						HasTemplate: true,
						ForEach:     []*RawForEach{{Name: "database", Expr: &RawExpr{Raw: "schema.spec.dbs"}}},
					},
				},
			},
			wantErr:    true,
			errContain: "conflicts with resource ID",
		},
		{
			name: "same iterator name across different nodes is fine",
			parsed: &ParsedRGD{
				Instance: validInstance(),
				Nodes: []*ParsedNode{
					{
						NodeMeta:    NodeMeta{ID: "workers", Template: validTemplate()},
						HasTemplate: true,
						ForEach:     []*RawForEach{{Name: "worker", Expr: &RawExpr{Raw: "schema.spec.workers"}}},
					},
					{
						NodeMeta:    NodeMeta{ID: "backups", Template: validTemplate()},
						HasTemplate: true,
						ForEach:     []*RawForEach{{Name: "worker", Expr: &RawExpr{Raw: "schema.spec.databases"}}},
					},
				},
			},
			wantErr: false,
		},
	}

	v := newValidator()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.Validate(tt.parsed)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContain != "" {
					require.Contains(t, err.Error(), tt.errContain)
				}
				require.True(t, IsTerminal(err), "validator errors must be terminal, got: %v", err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestLowerCamelCaseRegex_Cases(t *testing.T) {
	tests := []struct {
		name      string
		samples   []string
		wantMatch bool
	}{
		{
			name:      "valid lowerCamelCase samples",
			samples:   []string{"a", "ab", "aB", "myNode", "myNode123", "a1"},
			wantMatch: true,
		},
		{
			name:      "invalid lowerCamelCase samples",
			samples:   []string{"", "A", "AB", "MyNode", "1a", "_a", "a-b", "a_b", "a b", "a.b"},
			wantMatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, sample := range tt.samples {
				require.Equal(t, tt.wantMatch, lowerCamelCase.MatchString(sample), "%q lowerCamelCase match mismatch", sample)
			}
		})
	}
}

func TestUpperCamelCaseRegex_Cases(t *testing.T) {
	tests := []struct {
		name      string
		samples   []string
		wantMatch bool
	}{
		{
			name:      "valid UpperCamelCase samples",
			samples:   []string{"A", "AB", "MyNode", "MyNode123", "A1"},
			wantMatch: true,
		},
		{
			name:      "invalid UpperCamelCase samples",
			samples:   []string{"", "a", "ab", "myNode", "1A", "_A", "A-B", "A_B", "A B"},
			wantMatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, sample := range tt.samples {
				require.Equal(t, tt.wantMatch, upperCamelCase.MatchString(sample), "%q upperCamelCase match mismatch", sample)
			}
		})
	}
}

func TestKubernetesVersionRegex_Cases(t *testing.T) {
	tests := []struct {
		name      string
		samples   []string
		wantMatch bool
	}{
		{
			name:      "valid kubernetesVersion samples",
			samples:   []string{"v1", "v2", "v10", "v1alpha1", "v1alpha2", "v1alpha10", "v15alpha1", "v1beta1", "v1beta2"},
			wantMatch: true,
		},
		{
			name:      "invalid kubernetesVersion samples",
			samples:   []string{"", "v", "vvvv", "v1.1", "v1.1.1", "v1alpha", "valpha1", "alpha", "1alpha", "v1alpha1beta1"},
			wantMatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, sample := range tt.samples {
				require.Equal(t, tt.wantMatch, kubernetesVersion.MatchString(sample), "%q kubernetesVersion match mismatch", sample)
			}
		})
	}
}

func TestValidatorErrorMessages_Cases(t *testing.T) {
	tests := []struct {
		name   string
		parsed *ParsedRGD
	}{
		{
			name: "errors do not expose runtime internals",
			parsed: &ParsedRGD{
				Instance: validInstance(),
				Nodes:    []*ParsedNode{scalarNode("schema")},
			},
		},
	}

	v := newValidator()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.Validate(tt.parsed)
			require.Error(t, err)
			require.False(t, strings.Contains(err.Error(), "runtime."), "error should not contain runtime details")
			require.False(t, strings.Contains(err.Error(), "goroutine"), "error should not contain goroutine info")
		})
	}
}
