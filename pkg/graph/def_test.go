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

package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	memory2 "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
)

func TestGraphBuilder_Def(t *testing.T) {
	fakeResolver, fakeDiscovery := k8s.NewFakeResolver()
	builder := &Builder{
		schemaResolver: fakeResolver,
		restMapper:     restmapper.NewDeferredDiscoveryRESTMapper(memory2.NewMemCacheClient(fakeDiscovery)),
	}

	schemaOpt := generator.WithSchema(
		"Test", "v1alpha1",
		map[string]any{
			"name":     "string",
			"replicas": "integer",
			"names":    "[]string",
		},
		nil,
	)

	tests := []struct {
		name     string
		opts     []generator.ResourceGraphDefinitionOption
		errMsg   string
		validate func(t *testing.T, g *Graph)
	}{
		{
			name: "def referenced by a template becomes a dependency",
			opts: []generator.ResourceGraphDefinitionOption{
				schemaOpt,
				generator.WithDef("naming", map[string]any{
					"prefix": "${schema.spec.name + '-app'}",
					"static": "constant",
				}, nil, nil, nil),
				generator.WithResource("cm", map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "${naming.prefix}"},
					"data":       map[string]any{"v": "${naming.static}"},
				}, nil, nil),
			},
			validate: func(t *testing.T, g *Graph) {
				assert.Equal(t, NodeTypeDef, g.Nodes["naming"].Meta.Type)
				assert.Equal(t, []string{"naming"}, g.Nodes["cm"].Meta.Dependencies)
				assert.Equal(t, []string{"naming", "cm"}, g.TopologicalOrder)
			},
		},
		{
			name: "def field type mismatch against the consuming field is caught at build time",
			opts: []generator.ResourceGraphDefinitionOption{
				schemaOpt,
				generator.WithDef("computed", map[string]any{
					"count": "${schema.spec.replicas}",
				}, nil, nil, nil),
				generator.WithResource("cm", map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "cm"},
					"data":       map[string]any{"count": "${computed.count}"},
				}, nil, nil),
			},
			errMsg: "type mismatch",
		},
		{
			name: "def field converted to the consuming type is accepted",
			opts: []generator.ResourceGraphDefinitionOption{
				schemaOpt,
				generator.WithDef("computed", map[string]any{
					"count": "${schema.spec.replicas}",
				}, nil, nil, nil),
				generator.WithResource("cm", map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "cm"},
					"data":       map[string]any{"count": "${string(computed.count)}"},
				}, nil, nil),
			},
		},
		{
			name: "unknown field on a def is rejected",
			opts: []generator.ResourceGraphDefinitionOption{
				schemaOpt,
				generator.WithDef("computed", map[string]any{"a": "${schema.spec.name}"}, nil, nil, nil),
				generator.WithResource("cm", map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "${computed.missing}"},
				}, nil, nil),
			},
			errMsg: "missing",
		},
		{
			name: "def chained through another def and a resource is typed in order",
			opts: []generator.ResourceGraphDefinitionOption{
				schemaOpt,
				generator.WithDef("second", map[string]any{"v": "${first.v + '-2'}"}, nil, nil, nil),
				generator.WithDef("first", map[string]any{"v": "${schema.spec.name}"}, nil, nil, nil),
				generator.WithResource("cm", map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "${second.v}"},
				}, nil, nil),
			},
			validate: func(t *testing.T, g *Graph) {
				assert.Equal(t, []string{"first", "second", "cm"}, g.TopologicalOrder)
			},
		},
		{
			name: "def cycle is rejected",
			opts: []generator.ResourceGraphDefinitionOption{
				schemaOpt,
				generator.WithDef("a", map[string]any{"v": "${b.v}"}, nil, nil, nil),
				generator.WithDef("b", map[string]any{"v": "${a.v}"}, nil, nil, nil),
			},
			errMsg: "cycle",
		},
		{
			name: "def with forEach is a list consumed by a collection",
			opts: []generator.ResourceGraphDefinitionOption{
				schemaOpt,
				generator.WithDef("labels",
					map[string]any{"full": "${schema.spec.name + '-' + n}"},
					[]krov1alpha1.ForEachDimension{{"n": "${schema.spec.names}"}},
					nil, nil),
				generator.WithResourceCollection("cms", map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "${l.full}"},
				}, []krov1alpha1.ForEachDimension{{"l": "${labels}"}}, nil, nil),
			},
			validate: func(t *testing.T, g *Graph) {
				assert.Equal(t, []string{"labels"}, g.Nodes["cms"].Meta.Dependencies)
			},
		},
		{
			name: "def forEach iterator need not appear in an identity field",
			opts: []generator.ResourceGraphDefinitionOption{
				schemaOpt,
				generator.WithDef("pairs",
					map[string]any{"v": "${n}"},
					[]krov1alpha1.ForEachDimension{{"n": "${schema.spec.names}"}},
					nil, nil),
			},
		},
		{
			name: "def readyWhen binds each for a forEach def",
			opts: []generator.ResourceGraphDefinitionOption{
				schemaOpt,
				generator.WithDef("pairs",
					map[string]any{"v": "${n}"},
					[]krov1alpha1.ForEachDimension{{"n": "${schema.spec.names}"}},
					[]string{"${each.v != ''}"}, nil),
			},
		},
		{
			name: "def includeWhen is validated",
			opts: []generator.ResourceGraphDefinitionOption{
				schemaOpt,
				generator.WithDef("computed", map[string]any{"v": "${schema.spec.name}"},
					nil, nil, []string{"${schema.spec.replicas}"}),
			},
			errMsg: "includeWhen",
		},
		{
			name: "status field referencing a def is typed from the def",
			opts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]any{"name": "string", "replicas": "integer"},
					map[string]any{"prefix": "${computed.prefix}", "count": "${computed.count}"},
				),
				generator.WithDef("computed", map[string]any{
					"prefix": "${schema.spec.name + '-x'}",
					"count":  "${schema.spec.replicas}",
				}, nil, nil, nil),
			},
			validate: func(t *testing.T, g *Graph) {
				status := g.CRD.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
				assert.Equal(t, "string", status.Properties["prefix"].Type)
				assert.Equal(t, "integer", status.Properties["count"].Type)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rgd := generator.NewResourceGraphDefinition("test-def", tt.opts...)
			g, err := builder.NewResourceGraphDefinition(rgd, defaultRGDConfig)
			if tt.errMsg != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
				return
			}
			require.NoError(t, err)
			if tt.validate != nil {
				tt.validate(t, g)
			}
		})
	}
}

func TestGraphBuilder_DefShape(t *testing.T) {
	fakeResolver, fakeDiscovery := k8s.NewFakeResolver()
	builder := &Builder{
		schemaResolver: fakeResolver,
		restMapper:     restmapper.NewDeferredDiscoveryRESTMapper(memory2.NewMemCacheClient(fakeDiscovery)),
	}
	schemaOpt := generator.WithSchema("Test", "v1alpha1", map[string]any{"name": "string"}, nil)

	t.Run("def must be an object", func(t *testing.T) {
		rgd := generator.NewResourceGraphDefinition("test-def", schemaOpt)
		rgd.Spec.Resources = append(rgd.Spec.Resources, &krov1alpha1.Resource{
			ID:  "computed",
			Def: runtime.RawExtension{Raw: []byte(`"just a string"`)},
		})
		_, err := builder.NewResourceGraphDefinition(rgd, defaultRGDConfig)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be an object")
	})

	t.Run("def and template together are rejected", func(t *testing.T) {
		rgd := generator.NewResourceGraphDefinition("test-def", schemaOpt)
		rgd.Spec.Resources = append(rgd.Spec.Resources, &krov1alpha1.Resource{
			ID:       "both",
			Def:      runtime.RawExtension{Raw: []byte(`{"a":"b"}`)},
			Template: runtime.RawExtension{Raw: []byte(`{"kind":"ConfigMap"}`)},
		})
		_, err := builder.NewResourceGraphDefinition(rgd, defaultRGDConfig)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "only one of template, externalRef, or def")
	})
}
