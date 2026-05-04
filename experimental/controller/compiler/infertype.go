// infertype.go infers DeclTypes from template structure without schema information.
package compiler

import (
	apiservercel "k8s.io/apiserver/pkg/cel"

	"github.com/ellistarn/kro/experimental/controller/graph"
)

// ---------------------------------------------------------------------------
// Definition type inference (phase 2)
// ---------------------------------------------------------------------------

// inferObjectType builds a DeclType from a template map. Each key becomes a
// typed field. Nested maps produce nested ObjectTypes with path-based naming.
func InferObjectType(typeName string, tmpl map[string]any) *apiservercel.DeclType {
	fields := make(map[string]*apiservercel.DeclField, len(tmpl))
	for name, value := range tmpl {
		fieldPath := typeName + "." + name
		fieldType := InferFieldType(fieldPath, value)
		fields[name] = apiservercel.NewDeclField(name, fieldType, false, nil, nil)
	}
	return apiservercel.NewObjectType(typeName, fields)
}

// inferFieldType determines the CEL type of a template value.
func InferFieldType(path string, value any) *apiservercel.DeclType {
	switch v := value.(type) {
	case string:
		return InferStringType(v)
	case bool:
		return apiservercel.BoolType
	case int:
		return apiservercel.IntType
	case int64:
		return apiservercel.IntType
	case float64:
		// JSON numbers are float64. Integers that survive JSON round-trip
		// are typed as int for CEL compatibility.
		if v == float64(int64(v)) {
			return apiservercel.IntType
		}
		return apiservercel.DoubleType
	case map[string]any:
		return InferObjectType(path, v)
	case []any:
		if len(v) == 0 {
			return apiservercel.NewListType(apiservercel.DynType, -1)
		}
		elemType := InferFieldType(path+".@idx", v[0])
		return apiservercel.NewListType(elemType, -1)
	default:
		return apiservercel.DynType
	}
}

// inferStringType classifies a string value for type inference:
//   - Pure literal (no ${...}): string
//   - Standalone expression (${expr} is the entire string): dyn
//   - Embedded expression (text around ${expr}): string (interpolation always produces string)
func InferStringType(s string) *apiservercel.DeclType {
	dollars, _, start, end := graph.FindExpr(s, 0)
	if start < 0 {
		// No expression — pure literal string.
		return apiservercel.StringType
	}
	if start == 0 && end == len(s) && len(dollars) == 1 {
		// Standalone expression — type unknown without compilation.
		return apiservercel.DynType
	}
	// Embedded expression or deferred expression — string interpolation.
	return apiservercel.StringType
}
