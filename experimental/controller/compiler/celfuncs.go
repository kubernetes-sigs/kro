// celfuncs.go defines the custom CEL extension functions registered into the
// compilation environment: plural(), .ready(), simpleSchema.toOpenAPI(),
// .updated(), and .dependencies(). These are pure function factories — they
// produce cel.EnvOption values consumed by CompileGraphSpec and
// compileDeferredExpressions.
package compiler

import (
	"encoding/json"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"

	"github.com/gobuffalo/flect"

	"github.com/kubernetes-sigs/kro/pkg/cel/conversion"
	"github.com/kubernetes-sigs/kro/pkg/simpleschema"
)

// celPluralFunction returns CEL env options for the plural() function.
func celPluralFunction() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Function("plural",
			cel.Overload("plural_string",
				[]*cel.Type{cel.StringType},
				cel.StringType,
				cel.UnaryBinding(func(val ref.Val) ref.Val {
					s := val.Value().(string)
					return types.String(flect.Pluralize(s))
				}),
			),
		),
	}
}

// celFlagFunction returns CEL env options for a boolean flag member function
// (.ready() or .updated()). Both follow the same pattern: read a hidden field
// (flagField) from the receiver map, aggregate over collections, and handle
// optional receivers for lazy dependencies.
//
// For scalar nodes, reads flagField from the object map.
// For collections (forEach parents), returns true when ALL items have
// flagField == true. Empty collections are vacuously true.
// For optional receivers (lazy deps), returns optional.none() when absent,
// optional.of(bool) when present.
func celFlagFunction(funcName, flagField string) []cel.EnvOption {
	concreteImpl := func(val ref.Val) ref.Val {
		native, err := conversion.GoNativeType(val)
		if err != nil {
			return types.Bool(false)
		}
		switch obj := native.(type) {
		case map[string]any:
			flag, _ := obj[flagField].(bool)
			return types.Bool(flag)
		case []any:
			if len(obj) == 0 {
				return types.Bool(true) // empty collection is vacuously true
			}
			for _, item := range obj {
				m, ok := item.(map[string]any)
				if !ok {
					return types.Bool(false)
				}
				flag, _ := m[flagField].(bool)
				if !flag {
					return types.Bool(false)
				}
			}
			return types.Bool(true)
		default:
			return types.Bool(false)
		}
	}
	impl := func(val ref.Val) ref.Val {
		if opt, ok := val.(*types.Optional); ok {
			if !opt.HasValue() {
				return types.OptionalNone
			}
			return types.OptionalOf(concreteImpl(opt.GetValue()))
		}
		return concreteImpl(val)
	}
	return []cel.EnvOption{
		cel.Function(funcName,
			cel.MemberOverload("dyn_"+funcName,
				[]*cel.Type{cel.DynType},
				cel.DynType,
				cel.UnaryBinding(impl),
			),
		),
	}
}

// celSimpleSchemaFunction returns CEL env options for simpleSchema.toOpenAPI().
// Converts a SimpleSchema definition to an OpenAPI v3 schema for use in CRD specs.
// The first argument is a schema map with spec/status/types fields.
// The second argument is a resources list (used for context, currently unused).
func celSimpleSchemaFunction() []cel.EnvOption {
	impl := func(schemaVal, resourcesVal ref.Val) ref.Val {
		reg := types.NewEmptyRegistry()

		schemaNative, err := conversion.GoNativeType(schemaVal)
		if err != nil {
			return types.NewErr("simpleSchema.toOpenAPI: converting schema: %v", err)
		}

		schemaMap, ok := schemaNative.(map[string]any)
		if !ok {
			return types.NewErr("simpleSchema.toOpenAPI: schema must be a map, got %T", schemaNative)
		}

		specMap, _ := schemaMap["spec"].(map[string]any)
		if specMap == nil {
			specMap = schemaMap
		}
		customTypes, _ := schemaMap["types"].(map[string]any)

		openAPISchema, err := simpleschema.ToOpenAPISpec(specMap, customTypes)
		if err != nil {
			return types.NewErr("simpleSchema.toOpenAPI: %v", err)
		}

		// JSON round-trip: JSONSchemaProps → map[string]any
		jsonBytes, err := json.Marshal(openAPISchema)
		if err != nil {
			return types.NewErr("simpleSchema.toOpenAPI: marshaling: %v", err)
		}
		var result map[string]any
		if err := json.Unmarshal(jsonBytes, &result); err != nil {
			return types.NewErr("simpleSchema.toOpenAPI: unmarshaling: %v", err)
		}

		// Wrap with standard Kubernetes object structure
		fullSchema := map[string]any{
			"type": "object",
			"properties": map[string]any{
				"apiVersion": map[string]any{"type": "string"},
				"kind":       map[string]any{"type": "string"},
				"metadata":   map[string]any{"type": "object"},
				"spec":       result,
				"status": map[string]any{
					"type":                                 "object",
					"x-kubernetes-preserve-unknown-fields": true,
					"properties": map[string]any{
						"conditions": conditionsSchema(),
					},
				},
			},
		}

		// Convert status types if present (skip runtime ${} expressions)
		if statusMap, ok := schemaMap["status"].(map[string]any); ok && len(statusMap) > 0 {
			hasExpressions := false
			for _, v := range statusMap {
				if s, ok := v.(string); ok && strings.Contains(s, "${") {
					hasExpressions = true
					break
				}
			}
			if !hasExpressions {
				statusSchema, err := simpleschema.ToOpenAPISpec(statusMap, customTypes)
				if err != nil {
					return types.NewErr("simpleSchema.toOpenAPI: status: %v", err)
				}
				statusJSON, err := json.Marshal(statusSchema)
				if err != nil {
					return types.NewErr("simpleSchema.toOpenAPI: marshaling status: %v", err)
				}
				var statusResult map[string]any
				if err := json.Unmarshal(statusJSON, &statusResult); err != nil {
					return types.NewErr("simpleSchema.toOpenAPI: unmarshaling status: %v", err)
				}
				// Inject conditions schema into user-declared status for
				// proper SSA list-type semantics.
				injectConditionsSchema(statusResult)
				fullSchema["properties"].(map[string]any)["status"] = statusResult
			}
		}

		return reg.NativeToValue(fullSchema)
	}
	return []cel.EnvOption{
		cel.Function("simpleSchema.toOpenAPI",
			cel.Overload("simpleSchema_toOpenAPI",
				[]*cel.Type{cel.DynType, cel.DynType},
				cel.DynType,
				cel.BinaryBinding(impl),
			),
		),
	}
}

// conditionsSchema returns the conditions array schema with x-kubernetes-list-type:
// map keyed by "type". This enables SSA to handle individual conditions independently
// rather than treating the entire array as atomic — allowing separate field owners to
// manage different condition types (e.g., Compiled vs Ready) without conflict.
func conditionsSchema() map[string]any {
	return map[string]any{
		"type":                        "array",
		"x-kubernetes-list-type":      "map",
		"x-kubernetes-list-map-keys":  []any{"type"},
		"x-kubernetes-preserve-unknown-fields": true,
		"items": map[string]any{
			"type":                                 "object",
			"x-kubernetes-preserve-unknown-fields": true,
			"properties": map[string]any{
				"type":   map[string]any{"type": "string"},
				"status": map[string]any{"type": "string"},
			},
			"required": []any{"type"},
		},
	}
}

// injectConditionsSchema adds the conditions array schema with proper list-type
// annotations into an existing status schema. If conditions is already declared,
// it merges the list annotations without overwriting item definitions.
func injectConditionsSchema(statusSchema map[string]any) {
	props, ok := statusSchema["properties"].(map[string]any)
	if !ok {
		props = map[string]any{}
		statusSchema["properties"] = props
	}
	if _, exists := props["conditions"]; !exists {
		props["conditions"] = conditionsSchema()
	} else {
		// Conditions already declared — inject list annotations.
		cond, ok := props["conditions"].(map[string]any)
		if ok {
			cond["x-kubernetes-list-type"] = "map"
			cond["x-kubernetes-list-map-keys"] = []any{"type"}
		}
	}
}

// celDependenciesFunction returns CEL env options for the .dependencies()
// member function. The actual call never runs at runtime because the AST
// rewrite replaces it with a __kroDeps map lookup — this stub only exists
// to make CEL type-check pass.
func celDependenciesFunction() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Function("dependencies",
			cel.MemberOverload("dyn_dependencies",
				[]*cel.Type{cel.DynType},
				cel.ListType(cel.DynType),
				cel.UnaryBinding(func(val ref.Val) ref.Val {
					return types.NewDynamicList(types.DefaultTypeAdapter, []ref.Val{})
				}),
			),
		),
	}
}

// customCELFunctions returns all custom CEL extension functions registered
// into every compilation environment. Centralizes the registration list so
// that CompileGraphSpec (outer, refined, forEach inner-scope) and
// validateDeferredExprs all share a single definition.
func customCELFunctions() []cel.EnvOption {
	var opts []cel.EnvOption
	opts = append(opts, celPluralFunction()...)
	opts = append(opts, celFlagFunction("ready", "__ready")...)
	opts = append(opts, celSimpleSchemaFunction()...)
	opts = append(opts, celFlagFunction("updated", "__updated")...)
	opts = append(opts, celDependenciesFunction()...)
	return opts
}
