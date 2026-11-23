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

package library

import (
	"fmt"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	utiljson "k8s.io/apimachinery/pkg/util/json"

	"github.com/kubernetes-sigs/kro/pkg/cel/conversion"
	"github.com/kubernetes-sigs/kro/pkg/simpleschema"
)

// SimpleSchema registers simpleschema.toOpenAPI(fields, types), which converts
// SimpleSchema field declarations to an OpenAPI v3 object schema.
func SimpleSchema() cel.EnvOption {
	// Kubernetes object fields can have a named CEL type even when backed by a map.
	return cel.Function("simpleschema.toOpenAPI",
		cel.Overload("simpleschema.toOpenAPI_dyn_dyn",
			[]*cel.Type{cel.DynType, cel.DynType},
			cel.MapType(cel.StringType, cel.DynType),
			cel.BinaryBinding(toOpenAPI),
		),
	)
}

func toOpenAPI(fields, customTypes ref.Val) ref.Val {
	fieldMap, err := schemaMap(fields)
	if err != nil {
		return types.NewErr("simpleschema.toOpenAPI: fields: %v", err)
	}
	typeMap, err := schemaMap(customTypes)
	if err != nil {
		return types.NewErr("simpleschema.toOpenAPI: types: %v", err)
	}
	schema, err := simpleschema.ToOpenAPISpec(fieldMap, typeMap)
	if err != nil {
		return types.NewErr("simpleschema.toOpenAPI: %v", err)
	}
	// Round-trip through JSON to reject non-finite numbers and preserve integer defaults.
	raw, err := utiljson.Marshal(schema)
	if err != nil {
		return types.NewErr("simpleschema.toOpenAPI: %v", err)
	}
	var obj map[string]any
	if err := utiljson.Unmarshal(raw, &obj); err != nil {
		return types.NewErr("simpleschema.toOpenAPI: %v", err)
	}
	return types.DefaultTypeAdapter.NativeToValue(obj)
}

func schemaMap(value ref.Val) (map[string]any, error) {
	if value.Type() != types.MapType {
		return nil, fmt.Errorf("expected a map, got %s", value.Type().TypeName())
	}
	native, err := conversion.GoNativeType(value)
	if err != nil {
		return nil, err
	}
	return native.(map[string]any), nil
}
