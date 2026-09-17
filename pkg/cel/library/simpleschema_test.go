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

package library

import (
	"encoding/json"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextinstall "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/install"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

func newSimpleSchemaEnv(t *testing.T, opts ...cel.EnvOption) *cel.Env {
	t.Helper()
	env, err := cel.NewEnv(append([]cel.EnvOption{SimpleSchema()}, opts...)...)
	require.NoError(t, err)
	return env
}

// untypedSchema is what an expression-valued status field converts to: any
// value is accepted at that position.
var untypedSchema = map[string]any{"x-kubernetes-preserve-unknown-fields": true}

// emptyObjectSchema is what an absent spec or status block converts to.
var emptyObjectSchema = map[string]any{"type": "object"}

// rootWith returns the expected root schema for the given spec and status.
func rootWith(spec, status map[string]any) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"apiVersion": map[string]any{"type": "string"},
			"kind":       map[string]any{"type": "string"},
			"metadata":   map[string]any{"type": "object"},
			"spec":       spec,
			"status":     status,
		},
	}
}

// evalRoot evaluates expr and returns the result as a root schema map after
// checking it is a valid structural CRD schema — the check the API server
// performs on admission — so untyped (preserve-unknown-fields) properties
// without a type and every marker we emit are really accepted.
func evalRoot(t *testing.T, env *cel.Env, expr string, vars any) map[string]any {
	t.Helper()
	out := evalCELWithVars(t, env, expr, vars)
	root, ok := out.Value().(map[string]any)
	require.True(t, ok, "result must be a map, got %T", out.Value())

	raw, err := json.Marshal(root)
	require.NoError(t, err)
	var v1props extv1.JSONSchemaProps
	require.NoError(t, json.Unmarshal(raw, &v1props))
	convScheme := k8sruntime.NewScheme()
	apiextinstall.Install(convScheme)
	var internal apiextensions.JSONSchemaProps
	require.NoError(t, convScheme.Convert(&v1props, &internal, nil))
	structural, err := structuralschema.NewStructural(&internal)
	require.NoError(t, err)
	assert.Empty(t, structuralschema.ValidateStructural(nil, structural))
	return root
}

// specOf returns the spec sub-schema of a root schema.
func specOf(t *testing.T, root map[string]any) map[string]any {
	t.Helper()
	spec, ok := root["properties"].(map[string]any)["spec"].(map[string]any)
	require.True(t, ok, "root has no spec property: %v", root)
	return spec
}

// TestSimpleSchemaToOpenAPI covers how the schema block is read: which
// keys are converted, how they share custom types, how status expressions are
// handled, and which keys are ignored.
func TestSimpleSchemaToOpenAPI(t *testing.T) {
	env := newSimpleSchemaEnv(t)

	cases := []struct {
		name string
		expr string
		want map[string]any
	}{
		{
			name: "empty block",
			expr: `simpleschema.toOpenAPI({})`,
			want: rootWith(emptyObjectSchema, emptyObjectSchema),
		},
		{
			name: "spec only",
			expr: `simpleschema.toOpenAPI({"spec": {"name": "string | required=true"}})`,
			want: rootWith(
				map[string]any{
					"type":       "object",
					"required":   []any{"name"},
					"properties": map[string]any{"name": map[string]any{"type": "string"}},
				},
				emptyObjectSchema,
			),
		},
		{
			name: "custom types are shared by spec and status",
			expr: `simpleschema.toOpenAPI({
				"types": {"Condition": {"type": "string | required=true", "status": "string | required=true"}},
				"spec": {"expected": "Condition"},
				"status": {"items": "integer", "conditions": "[]Condition"}
			})`,
			want: rootWith(
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"expected": map[string]any{
							"type":     "object",
							"required": []any{"status", "type"},
							"properties": map[string]any{
								"type":   map[string]any{"type": "string"},
								"status": map[string]any{"type": "string"},
							},
						},
					},
				},
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"items": map[string]any{"type": "integer"},
						"conditions": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type":     "object",
								"required": []any{"status", "type"},
								"properties": map[string]any{
									"type":   map[string]any{"type": "string"},
									"status": map[string]any{"type": "string"},
								},
							},
						},
					},
				},
			),
		},
		{
			name: "status expressions become untyped properties",
			expr: `simpleschema.toOpenAPI({
				"spec": {"name": "string"},
				"status": {
					"ready": "${deployment.status.readyReplicas > 0}",
					"url": "http://${service.spec.clusterIP}",
					"conditions": ["${runtime.newCondition({type: 'Ready', status: 'True'})}"],
					"count": "integer"
				}
			})`,
			want: rootWith(
				map[string]any{
					"type":       "object",
					"properties": map[string]any{"name": map[string]any{"type": "string"}},
				},
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"ready":      untypedSchema,
						"url":        untypedSchema,
						"conditions": untypedSchema,
						"count":      map[string]any{"type": "integer"},
					},
				},
			),
		},
		{
			name: "null and CRD-descriptor keys are ignored",
			expr: `simpleschema.toOpenAPI({
				"apiVersion": "example.com/v1alpha1",
				"kind": "App",
				"group": "example.com",
				"scope": "Namespaced",
				"additionalPrinterColumns": [{"name": "Age", "type": "date", "jsonPath": ".metadata.creationTimestamp"}],
				"spec": {"name": "string"},
				"types": null,
				"status": null
			})`,
			want: rootWith(
				map[string]any{
					"type":       "object",
					"properties": map[string]any{"name": map[string]any{"type": "string"}},
				},
				emptyObjectSchema,
			),
		},
		{
			// An RGD may declare a Kind with no spec, types or status at all;
			// its spec.schema must still convert rather than be mistaken for a
			// bare field map.
			name: "spec-less RGD schema block",
			expr: `simpleschema.toOpenAPI({"apiVersion": "v1alpha1", "kind": "Singleton", "group": "kro.run", "scope": "Namespaced"})`,
			want: rootWith(emptyObjectSchema, emptyObjectSchema),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, evalRoot(t, env, tc.expr, cel.NoVars()))
		})
	}
}

// TestSimpleSchemaToOpenAPISpecBlockShapes checks that SimpleSchema features
// survive the CEL boundary with the shape the RGD controller writes into
// instance CRDs, decoded into native CEL values: int for integral numbers,
// nested maps for objects, lists for enum/required.
func TestSimpleSchemaToOpenAPISpecBlockShapes(t *testing.T) {
	env := newSimpleSchemaEnv(t)

	cases := []struct {
		name  string
		spec  string // CEL map literal for the spec block
		types string // CEL map literal for the types block
		want  map[string]any
	}{
		{
			name: "atomic types",
			spec: `{'name': 'string', 'count': 'integer', 'ratio': 'float', 'enabled': 'boolean'}`,
			want: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":    map[string]any{"type": "string"},
					"count":   map[string]any{"type": "integer"},
					"ratio":   map[string]any{"type": "number"},
					"enabled": map[string]any{"type": "boolean"},
				},
			},
		},
		{
			name: "markers: required, default, description, minimum, maximum",
			spec: `{
				'name': 'string | required=true default=web description="Resource name"',
				'replicas': 'integer | default=3 minimum=1 maximum=10'
			}`,
			want: map[string]any{
				"type":     "object",
				"required": []any{"name"},
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"default":     "web",
						"description": "Resource name",
					},
					"replicas": map[string]any{
						"type":    "integer",
						"default": int64(3),
						"minimum": int64(1),
						"maximum": int64(10),
					},
				},
			},
		},
		{
			name: "required list is sorted",
			spec: `{'zeta': 'string | required=true', 'alpha': 'string | required=true', 'mid': 'string | required=true'}`,
			want: map[string]any{
				"type":     "object",
				"required": []any{"alpha", "mid", "zeta"},
				"properties": map[string]any{
					"zeta":  map[string]any{"type": "string"},
					"alpha": map[string]any{"type": "string"},
					"mid":   map[string]any{"type": "string"},
				},
			},
		},
		{
			name: "enum and validation markers",
			spec: `{
				'env': 'string | enum="dev,prod"',
				'level': 'integer | enum="1,2,3"',
				'id': 'string | immutable=true'
			}`,
			want: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"env":   map[string]any{"type": "string", "enum": []any{"dev", "prod"}},
					"level": map[string]any{"type": "integer", "enum": []any{int64(1), int64(2), int64(3)}},
					"id": map[string]any{
						"type": "string",
						"x-kubernetes-validations": []any{
							map[string]any{"rule": "self == oldSelf", "message": "field is immutable"},
						},
					},
				},
			},
		},
		{
			name: "nested inline object propagates default",
			spec: `{'config': {'host': 'string', 'port': 'integer | default=8080'}}`,
			want: map[string]any{
				"type":    "object",
				"default": map[string]any{},
				"properties": map[string]any{
					"config": map[string]any{
						"type":    "object",
						"default": map[string]any{},
						"properties": map[string]any{
							"host": map[string]any{"type": "string"},
							"port": map[string]any{"type": "integer", "default": int64(8080)},
						},
					},
				},
			},
		},
		{
			name: "arrays and maps",
			spec: `{'tags': '[]string', 'labels': 'map[string]string', 'matrix': '[][]integer'}`,
			want: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tags": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "string"},
					},
					"labels": map[string]any{
						"type":                 "object",
						"additionalProperties": map[string]any{"type": "string"},
					},
					"matrix": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type":  "array",
							"items": map[string]any{"type": "integer"},
						},
					},
				},
			},
		},
		{
			name: "object type preserves unknown fields",
			spec: `{'raw': 'object'}`,
			want: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"raw": map[string]any{
						"type":                                 "object",
						"x-kubernetes-preserve-unknown-fields": true,
					},
				},
			},
		},
		{
			name:  "custom types: struct and required alias",
			spec:  `{'owner': 'Person', 'nickname': 'Name'}`,
			types: `{'Person': {'name': 'Name', 'age': 'integer'}, 'Name': 'string | required=true'}`,
			want: map[string]any{
				"type":     "object",
				"required": []any{"nickname"},
				"properties": map[string]any{
					"nickname": map[string]any{"type": "string"},
					"owner": map[string]any{
						"type":     "object",
						"required": []any{"name"},
						"properties": map[string]any{
							"name": map[string]any{"type": "string"},
							"age":  map[string]any{"type": "integer"},
						},
					},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			types := tc.types
			if types == "" {
				types = "{}"
			}
			root := evalRoot(t, env, `simpleschema.toOpenAPI({"spec": `+tc.spec+`, "types": `+types+`})`, cel.NoVars())
			assert.Equal(t, tc.want, specOf(t, root))
		})
	}
}

// TestSimpleSchemaToOpenAPIResultIsCELMap verifies the result behaves as
// a regular CEL map so authors can post-process it (index, has(), size(), or
// pull out a sub-schema to compose a root by hand).
func TestSimpleSchemaToOpenAPIResultIsCELMap(t *testing.T) {
	env := newSimpleSchemaEnv(t)

	const block = `{'spec': {'name': 'string | required=true', 'replicas': 'integer | default=3'}}`
	evalTrue(t, env, `simpleschema.toOpenAPI(`+block+`).type == 'object'`)
	evalTrue(t, env, `simpleschema.toOpenAPI(`+block+`).properties.spec.properties.name.type == 'string'`)
	evalTrue(t, env, `simpleschema.toOpenAPI(`+block+`).properties.spec.required == ['name']`)
	evalTrue(t, env, `simpleschema.toOpenAPI(`+block+`).properties.spec.properties.replicas.default == 3`)
	evalTrue(t, env, `size(simpleschema.toOpenAPI(`+block+`).properties.spec.properties) == 2`)
	evalTrue(t, env, `simpleschema.toOpenAPI(`+block+`).properties.status == {'type': 'object'}`)
	evalTrue(t, env, `!has(simpleschema.toOpenAPI({'spec': {}}).properties.spec.properties)`)
}

// TestSimpleSchemaToOpenAPIOutputType pins the declared return type: dyn,
// so the result is assignable to any typed field of a resource template.
func TestSimpleSchemaToOpenAPIOutputType(t *testing.T) {
	env := newSimpleSchemaEnv(t)

	ast, iss := env.Compile(`simpleschema.toOpenAPI({'spec': {'name': 'string'}})`)
	require.NoError(t, iss.Err())
	assert.Equal(t, cel.DynType.String(), ast.OutputType().String())
}

// TestSimpleSchemaToOpenAPIObjectTypedArgCompiles verifies that an
// argument typed as an opaque object (the shape a typed environment gives an
// RGD's spec.schema, whose blocks are x-kubernetes-preserve-unknown-fields
// objects) passes type-checking. A map(string, dyn) signature would reject it.
func TestSimpleSchemaToOpenAPIObjectTypedArgCompiles(t *testing.T) {
	env := newSimpleSchemaEnv(t, cel.Variable("schemaBlock", cel.ObjectType("rgd.schema")))

	pAst, iss := env.Parse(`simpleschema.toOpenAPI(schemaBlock)`)
	require.NoError(t, iss.Err())
	_, iss = env.Check(pAst)
	require.NoError(t, iss.Err(), "toOpenAPI must type-check against an object-typed argument")
}

// TestSimpleSchemaToOpenAPIDynVariable is the runtime path kro uses: the
// block is a dyn variable bound to a native map[string]interface{} read from an
// unstructured object.
func TestSimpleSchemaToOpenAPIDynVariable(t *testing.T) {
	env := newSimpleSchemaEnv(t, cel.Variable("kindSpec", cel.DynType))

	block := map[string]any{
		"kind": "App",
		"spec": map[string]any{
			"name":     "string | required=true",
			"replicas": "integer | default=1 minimum=0",
			"owner":    "Owner",
			"labels":   "map[string]string",
			"nested":   map[string]any{"enabled": "boolean | default=true"},
		},
		"types": map[string]any{
			"Owner": map[string]any{"team": "string | required=true", "email": "string"},
		},
		"status": map[string]any{"ready": "boolean"},
	}
	vars := map[string]any{"kindSpec": block}

	got := evalRoot(t, env, `simpleschema.toOpenAPI(kindSpec)`, vars)

	want := rootWith(
		map[string]any{
			"type":     "object",
			"required": []any{"name"},
			"properties": map[string]any{
				"name":     map[string]any{"type": "string"},
				"replicas": map[string]any{"type": "integer", "default": int64(1), "minimum": int64(0)},
				"labels": map[string]any{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": "string"},
				},
				"owner": map[string]any{
					"type":     "object",
					"required": []any{"team"},
					"properties": map[string]any{
						"team":  map[string]any{"type": "string"},
						"email": map[string]any{"type": "string"},
					},
				},
				"nested": map[string]any{
					"type":    "object",
					"default": map[string]any{},
					"properties": map[string]any{
						"enabled": map[string]any{"type": "boolean", "default": true},
					},
				},
			},
		},
		map[string]any{
			"type":       "object",
			"properties": map[string]any{"ready": map[string]any{"type": "boolean"}},
		},
	)
	assert.Equal(t, want, got)

	// The input must not be mutated by the conversion.
	assert.Equal(t, "string | required=true", block["spec"].(map[string]any)["name"])
}

// TestSimpleSchemaToOpenAPIKindDefinition converts the schema block of a
// Graph-native Kind definition — spec, status and custom types written in
// SimpleSchema, the way the graph-native RGD design declares its own CRD. Every
// SimpleSchema feature used there (custom types, array-of-map, unquoted pattern,
// object/array defaults, required markers) must survive, and the result must be
// a valid structural schema.
func TestSimpleSchemaToOpenAPIKindDefinition(t *testing.T) {
	env := newSimpleSchemaEnv(t)

	root := evalRoot(t, env, `simpleschema.toOpenAPI({
		"types": {
			"Node": {
				"id": "string",
				"template": "object",
				"ref": "object",
				"forEach": "[]map[string]string",
				"includeWhen": "[]string",
				"readyWhen": "[]string"
			},
			"PrinterColumn": {
				"name": "string | required=true",
				"type": "string | required=true",
				"jsonPath": "string | required=true",
				"priority": "integer"
			},
			"Condition": {
				"type": "string | required=true",
				"status": "string | required=true",
				"reason": "string",
				"message": "string",
				"lastTransitionTime": "string",
				"observedGeneration": "integer"
			}
		},
		"spec": {
			"scope": "string | default=Namespaced",
			"schema": {
				"apiVersion": "string | required=true pattern=^[a-z][a-z0-9.-]*/[a-z][a-z0-9]*$",
				"kind": "string | required=true",
				"spec": "object",
				"status": "object | default={}",
				"types": "object"
			},
			"nodes": "[]Node",
			"additionalPrinterColumns": "[]PrinterColumn | default=[]"
		},
		"status": {
			"items": "integer",
			"conditions": "[]Condition"
		}
	})`, cel.NoVars())

	props := root["properties"].(map[string]any)
	spec := props["spec"].(map[string]any)
	specProps := spec["properties"].(map[string]any)
	assert.Equal(t, map[string]any{}, spec["default"], "a child default propagates default: {} to the parent")
	assert.Equal(t, map[string]any{"type": "string", "default": "Namespaced"}, specProps["scope"])

	schema := specProps["schema"].(map[string]any)
	assert.Equal(t, []any{"apiVersion", "kind"}, schema["required"])
	schemaProps := schema["properties"].(map[string]any)
	assert.Equal(t, map[string]any{"type": "string", "pattern": "^[a-z][a-z0-9.-]*/[a-z][a-z0-9]*$"}, schemaProps["apiVersion"])
	assert.Equal(t, map[string]any{"type": "object", "x-kubernetes-preserve-unknown-fields": true, "default": map[string]any{}}, schemaProps["status"])

	nodes := specProps["nodes"].(map[string]any)
	nodeProps := nodes["items"].(map[string]any)["properties"].(map[string]any)
	assert.Equal(t, map[string]any{
		"type":  "array",
		"items": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
	}, nodeProps["forEach"])

	columns := specProps["additionalPrinterColumns"].(map[string]any)
	assert.Equal(t, []any{}, columns["default"])
	assert.Equal(t, []any{"jsonPath", "name", "type"}, columns["items"].(map[string]any)["required"])

	status := props["status"].(map[string]any)
	statusProps := status["properties"].(map[string]any)
	assert.Equal(t, map[string]any{"type": "integer"}, statusProps["items"])
	conditionProps := statusProps["conditions"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	assert.Len(t, conditionProps, 6)
	assert.Equal(t, map[string]any{"type": "integer"}, conditionProps["observedGeneration"])
}

// TestSimpleSchemaToOpenAPIRGDSchema feeds the function an actual
// ResourceGraphDefinition spec.schema block, as a Graph reading RGDs would
// (`simpleschema.toOpenAPI(rgd.spec.schema)`), and shows how kro's default
// instance status fields are added on top with deepMerge before conversion.
func TestSimpleSchemaToOpenAPIRGDSchema(t *testing.T) {
	env := newSimpleSchemaEnv(t, Maps(), cel.Variable("rgdSchema", cel.DynType))

	vars := map[string]any{"rgdSchema": map[string]any{
		"apiVersion": "v1alpha1",
		"kind":       "WebApp",
		"group":      "kro.run",
		"scope":      "Namespaced",
		"spec": map[string]any{
			"name":     "string | required=true",
			"replicas": "integer | default=1",
			"owner":    "Owner",
		},
		"types": map[string]any{
			"Owner": map[string]any{"team": "string"},
		},
		"status": map[string]any{
			"availableReplicas": "${deployment.status.availableReplicas}",
			"url":               "http://${service.spec.clusterIP}",
			"conditions":        []any{"${runtime.newCondition({type: 'AppReady', status: 'True', reason: '', message: ''})}"},
		},
	}}

	t.Run("as-is", func(t *testing.T) {
		root := evalRoot(t, env, `simpleschema.toOpenAPI(rgdSchema)`, vars)

		spec := specOf(t, root)
		assert.Equal(t, []any{"name"}, spec["required"])
		specProps := spec["properties"].(map[string]any)
		assert.Equal(t, map[string]any{"type": "integer", "default": int64(1)}, specProps["replicas"])
		assert.Equal(t, map[string]any{"type": "string"}, specProps["owner"].(map[string]any)["properties"].(map[string]any)["team"])

		status := root["properties"].(map[string]any)["status"].(map[string]any)
		assert.Equal(t, map[string]any{
			"availableReplicas": untypedSchema,
			"url":               untypedSchema,
			"conditions":        untypedSchema,
		}, status["properties"], "RGD status values are expressions and cannot be typed without the referenced schemas")
	})

	t.Run("with kro default status fields merged in", func(t *testing.T) {
		// deepMerge replaces the expression-valued conditions list with a typed
		// SimpleSchema declaration and adds state, mirroring crd.SetCRDStatus.
		const expr = `simpleschema.toOpenAPI(rgdSchema.deepMerge({
			"status": {"state": "string", "conditions": "[]KroCondition"},
			"types": {"KroCondition": {"type": "string", "status": "string", "reason": "string", "message": "string", "lastTransitionTime": "string", "observedGeneration": "integer"}}
		}))`
		root := evalRoot(t, env, expr, vars)

		statusProps := root["properties"].(map[string]any)["status"].(map[string]any)["properties"].(map[string]any)
		assert.Equal(t, map[string]any{"type": "string"}, statusProps["state"])
		assert.Equal(t, untypedSchema, statusProps["availableReplicas"], "author status fields are preserved")
		conditions := statusProps["conditions"].(map[string]any)
		assert.Equal(t, "array", conditions["type"])
		assert.Len(t, conditions["items"].(map[string]any)["properties"], 6)
		assert.Contains(t, specOf(t, root)["properties"], "owner", "existing custom types survive the merge")
	})
}

func TestSimpleSchemaToOpenAPIErrors(t *testing.T) {
	env := newSimpleSchemaEnv(t, cel.Variable("anyVal", cel.DynType))

	cases := []struct {
		name    string
		expr    string
		vars    map[string]any
		wantErr string
	}{
		{
			name:    "block is a string",
			expr:    `simpleschema.toOpenAPI('spec: {}')`,
			wantErr: "simpleschema.toOpenAPI: schema argument must be a map with string keys, got string",
		},
		{
			name:    "block is a list",
			expr:    `simpleschema.toOpenAPI([{'spec': {}}])`,
			wantErr: "simpleschema.toOpenAPI: schema argument must be a map with string keys, got list",
		},
		{
			name:    "block is null",
			expr:    `simpleschema.toOpenAPI(anyVal)`,
			vars:    map[string]any{"anyVal": nil},
			wantErr: "simpleschema.toOpenAPI: schema argument must be a map with string keys, got null_type",
		},
		{
			name:    "non-string map key",
			expr:    `simpleschema.toOpenAPI({1: {}})`,
			wantErr: "simpleschema.toOpenAPI: schema argument must be a map with string keys: map key must be string",
		},
		{
			name:    "a bare field map is not a schema block",
			expr:    `simpleschema.toOpenAPI({"name": "string | required=true"})`,
			wantErr: `simpleschema.toOpenAPI: schema block has none of the keys "spec", "types" or "status"; wrap SimpleSchema fields in {"spec": {...}}`,
		},
		{
			name:    "spec is not a map",
			expr:    `simpleschema.toOpenAPI({"spec": "string"})`,
			wantErr: `simpleschema.toOpenAPI: schema block key "spec" must be a map with string keys, got string`,
		},
		{
			name:    "types is a list",
			expr:    `simpleschema.toOpenAPI({"spec": {}, "types": []})`,
			wantErr: `simpleschema.toOpenAPI: schema block key "types" must be a map with string keys, got list`,
		},
		{
			name:    "spec does not accept expressions",
			expr:    `simpleschema.toOpenAPI({"spec": {"ready": "${deployment.status.readyReplicas > 0}"}})`,
			wantErr: "simpleschema.toOpenAPI: spec: field ready:",
		},
		{
			name:    "unknown type",
			expr:    `simpleschema.toOpenAPI({"spec": {"name": "strng"}})`,
			wantErr: "simpleschema.toOpenAPI: spec: field name: unknown type: strng",
		},
		{
			name:    "unknown marker",
			expr:    `simpleschema.toOpenAPI({"spec": {"name": "string | requird=true"}})`,
			wantErr: `simpleschema.toOpenAPI: spec: field name: invalid marker key "requird": unknown marker`,
		},
		{
			name:    "field value is not a string or map",
			expr:    `simpleschema.toOpenAPI({"spec": {"name": 1}})`,
			wantErr: "simpleschema.toOpenAPI: spec: field name: unexpected type: int64",
		},
		{
			name:    "cyclic custom types are attributed to types",
			expr:    `simpleschema.toOpenAPI({"spec": {"a": "A"}, "types": {"A": {"b": "B"}, "B": {"a": "A"}}})`,
			wantErr: "simpleschema.toOpenAPI: types: cyclic dependency in type",
		},
		{
			name:    "invalid custom type is attributed to types even without a spec block",
			expr:    `simpleschema.toOpenAPI({"status": {"a": "A"}, "types": {"A": "string | requird=true"}})`,
			wantErr: "simpleschema.toOpenAPI: types: building schema for A:",
		},
		{
			name:    "enum on unsupported type",
			expr:    `simpleschema.toOpenAPI({"spec": {"flag": "boolean | enum=\"true,false\""}})`,
			wantErr: "simpleschema.toOpenAPI: spec: field flag: enum values only supported for string and integer types",
		},
		{
			name:    "status type errors are attributed to status",
			expr:    `simpleschema.toOpenAPI({"status": {"items": "Foo"}})`,
			wantErr: "simpleschema.toOpenAPI: status: field items: unknown type: Foo",
		},
		{
			name:    "status list that is not an expression list",
			expr:    `simpleschema.toOpenAPI({"status": {"items": ["integer"]}})`,
			wantErr: "simpleschema.toOpenAPI: status: field items: a list value must contain only CEL expressions",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ast, iss := env.Compile(tc.expr)
			require.NoError(t, iss.Err(), "compile %q", tc.expr)
			prg, err := env.Program(ast)
			require.NoError(t, err)

			vars := tc.vars
			if vars == nil {
				vars = map[string]any{"anyVal": map[string]any{}}
			}
			out, _, err := prg.Eval(vars)
			require.Error(t, err, "expected eval error for %q", tc.expr)
			require.True(t, types.IsError(out), "expected a CEL error value, got %v", out)
			assert.Contains(t, out.(*types.Err).String(), tc.wantErr)
		})
	}
}

// TestSimpleSchemaToOpenAPIWrongArity checks the checker rejects calls
// that do not match the single (dyn) overload; in particular a two-argument
// (spec, types) call is not an overload.
func TestSimpleSchemaToOpenAPIWrongArity(t *testing.T) {
	env := newSimpleSchemaEnv(t)

	for _, expr := range []string{
		`simpleschema.toOpenAPI()`,
		`simpleschema.toOpenAPI({'name': 'string'}, {})`,
		`simpleschema.toOpenAPI({'spec': {}}, {}, {})`,
	} {
		_, iss := env.Compile(expr)
		require.Error(t, iss.Err(), "expected compile error for %q", expr)
		assert.Contains(t, iss.Err().Error(), "found no matching overload")
	}
}

func TestSimpleSchemaLibraryName(t *testing.T) {
	lib := &simpleSchemaLibrary{}
	assert.Equal(t, "kro.simpleschema", lib.LibraryName())
	assert.Nil(t, lib.ProgramOptions())
}
