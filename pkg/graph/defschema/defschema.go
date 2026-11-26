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

// Package defschema infers the OpenAPI schema of a def payload: computed data
// whose fields are literals or CEL expressions. It is shared by the RGD builder
// and the Graph compiler so both type a def the same way.
package defschema

import (
	"maps"
	"strings"

	"github.com/google/cel-go/cel"
	"k8s.io/kube-openapi/pkg/validation/spec"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph/parser"
	"github.com/kubernetes-sigs/kro/pkg/graph/schema"
)

// Infer returns the schema of a def payload. It flattens the payload into a map
// from field path to CEL type, then builds the schema with
// schema.GenerateSchemaFromCELTypes. An expression field takes its checked
// output type from exprTypes, keyed by the path format of
// parser.ParseSchemalessResource. A literal takes the type of its value.
func Infer(payload map[string]any, exprTypes map[string]*cel.Type, provider *krocel.DeclTypeProvider) (*spec.Schema, error) {
	types := make(map[string]*cel.Type)
	collect(payload, "", exprTypes, types)
	props, err := schema.GenerateSchemaFromCELTypes(types, provider)
	if err != nil {
		return nil, err
	}
	sch, err := schema.ConvertJSONSchemaPropsToSpecSchema(props)
	if err != nil {
		return nil, err
	}
	markDyn(sch)
	return sch, nil
}

// markDyn rewrites every untyped preserve-unknown-fields node to the marker
// that the CEL decl type conversion maps to dyn. A schema with no type is
// otherwise dropped from its parent, so a dyn field would be undeclared.
func markDyn(s *spec.Schema) {
	if len(s.Type) == 0 && len(s.Properties) == 0 && s.Items == nil {
		*s = spec.Schema{}
		s.AddExtension("x-kubernetes-int-or-string", true)
		return
	}
	for k, p := range s.Properties {
		markDyn(&p)
		s.Properties[k] = p
	}
	if s.Items != nil && s.Items.Schema != nil {
		markDyn(s.Items.Schema)
	}
}

func collect(v any, path string, exprTypes, out map[string]*cel.Type) {
	switch val := v.(type) {
	case map[string]any:
		if len(val) == 0 && path != "" {
			out[path] = cel.MapType(cel.StringType, cel.DynType)
		}
		for k, child := range val {
			collect(child, parser.JoinPathAndFieldName(path, k), exprTypes, out)
		}
	case []any:
		if len(val) == 0 || !uniform(val, path, exprTypes) {
			out[path] = cel.ListType(cel.DynType)
			return
		}
		for i, item := range val {
			collect(item, parser.JoinPathAndIndex(path, i), exprTypes, out)
		}
	case string:
		if t, ok := exprTypes[path]; ok {
			out[path] = t
		} else {
			out[path] = cel.StringType
		}
	case bool:
		out[path] = cel.BoolType
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, uintptr:
		out[path] = cel.IntType
	case float32, float64:
		out[path] = cel.DoubleType
	default:
		out[path] = cel.DynType
	}
}

// uniform reports whether every element of an array has the same shape, so one
// element schema describes them all.
func uniform(items []any, path string, exprTypes map[string]*cel.Type) bool {
	var first map[string]string
	for i, item := range items {
		prefix := parser.JoinPathAndIndex(path, i)
		types := make(map[string]*cel.Type)
		collect(item, prefix, exprTypes, types)
		shape := make(map[string]string, len(types))
		for p, t := range types {
			shape[strings.TrimPrefix(p, prefix)] = t.String()
		}
		if i == 0 {
			first = shape
		} else if !maps.Equal(first, shape) {
			return false
		}
	}
	return true
}
