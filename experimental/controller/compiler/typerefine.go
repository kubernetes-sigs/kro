// typerefine.go narrows definition and iterator types using compiled expression return types.
package compiler

import (
	"github.com/google/cel-go/cel"
	apiservercel "k8s.io/apiserver/pkg/cel"

	"github.com/ellistarn/kro/experimental/controller/graph"
	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
)

// refineDefTypes narrows definition types using expression return types from
// the first compilation pass. For each def node, standalone expression fields
// that were initially typed as dyn are narrowed to their compiled return type.
//
// Returns a new TypeSource with narrowed definitions, or nil if nothing changed.
func refineDefTypes(nodes []graph.Node, ts *TypeSource, exprTypes map[string]*cel.Type) *TypeSource {
	narrowed := false
	newDefTypes := make(map[string]*apiservercel.DeclType, len(ts.DefinitionTypes))

	for _, node := range nodes {
		if node.Type() != graph.NodeTypeDef {
			continue
		}
		body := node.Payload()
		if body == nil {
			continue
		}
		origDT, ok := ts.DefinitionTypes[node.ID]
		if !ok {
			continue
		}

		typeName := krocel.TypeNamePrefix + node.ID
		var fieldNarrowed bool
		refinedDT := narrowObjectTypeTracked(typeName, body, exprTypes, &fieldNarrowed)
		if fieldNarrowed {
			newDefTypes[node.ID] = refinedDT
			narrowed = true
		} else {
			newDefTypes[node.ID] = origDT
		}
	}

	if !narrowed {
		return nil
	}

	// Narrow forEach iterator variables from the collection expression's
	// element type. If the collection returns list(T), the iterator is T.
	// Remove narrowed iterators from untypedIDs.
	narrowedIterators := make(map[string]*cel.Type) // varName → element cel.Type
	for _, node := range nodes {
		if node.ForEach == nil {
			continue
		}
		dollars, innerExpr, start, end := graph.FindExpr(node.ForEach.Expr, 0)
		if start < 0 || len(dollars) != 1 || start != 0 || end != len(node.ForEach.Expr) {
			continue
		}
		ct, ok := exprTypes[innerExpr]
		if !ok || ct == cel.DynType || ct == cel.AnyType {
			continue
		}
		params := ct.Parameters()
		if len(params) == 1 {
			// list(T) — iterator should be T
			elemType := params[0]
			if elemType != cel.DynType && elemType != cel.AnyType {
				narrowedIterators[node.ForEach.VarName] = elemType
				narrowed = true
			}
		}
	}

	// Filter narrowed iterators out of untypedIDs.
	var filteredUntypedIDs []string
	for _, id := range ts.UntypedIDs {
		if _, isNarrowed := narrowedIterators[id]; !isNarrowed {
			filteredUntypedIDs = append(filteredUntypedIDs, id)
		}
	}

	// Build a new TypeSource with narrowed definitions. Resource schemas
	// and other fields are unchanged.
	return &TypeSource{
		ResourceSchemas:     ts.ResourceSchemas,
		forEachSchemas:      ts.forEachSchemas,
		resourceCollections: ts.resourceCollections,
		DefinitionTypes:     newDefTypes,
		ForEachDefinitions:  ts.ForEachDefinitions,
		UntypedIDs:          filteredUntypedIDs,
		listIDs:             ts.listIDs,
		UnresolvedGVKs:      ts.UnresolvedGVKs,
		narrowedIterators:   narrowedIterators,
	}
}

// narrowObjectTypeTracked rebuilds a DeclType for a def body, replacing dyn fields
// with their expression return types where known. Sets *narrowed to true
// if any field was narrowed from dyn to a concrete type.
func narrowObjectTypeTracked(typeName string, body map[string]any, exprTypes map[string]*cel.Type, narrowed *bool) *apiservercel.DeclType {
	fields := make(map[string]*apiservercel.DeclField, len(body))
	for name, value := range body {
		fieldPath := typeName + "." + name
		fieldType := narrowFieldType(fieldPath, value, exprTypes, narrowed)
		fields[name] = apiservercel.NewDeclField(name, fieldType, false, nil, nil)
	}
	return apiservercel.NewObjectType(typeName, fields)
}

// narrowFieldType determines the CEL type of a field value, using expression
// return types to narrow standalone expressions from dyn to their actual type.
func narrowFieldType(path string, value any, exprTypes map[string]*cel.Type, narrowed *bool) *apiservercel.DeclType {
	switch v := value.(type) {
	case string:
		return narrowStringType(v, exprTypes, narrowed)
	case map[string]any:
		return narrowObjectTypeTracked(path, v, exprTypes, narrowed)
	case []any:
		if len(v) == 0 {
			return apiservercel.NewListType(apiservercel.DynType, -1)
		}
		elemType := narrowFieldType(path+".@idx", v[0], exprTypes, narrowed)
		return apiservercel.NewListType(elemType, -1)
	default:
		return InferFieldType(path, value)
	}
}

// narrowStringType is like inferStringType but narrows standalone expressions
// using the expression's compiled return type.
func narrowStringType(s string, exprTypes map[string]*cel.Type, narrowed *bool) *apiservercel.DeclType {
	dollars, expr, start, end := graph.FindExpr(s, 0)
	if start < 0 {
		return apiservercel.StringType
	}
	if start == 0 && end == len(s) && len(dollars) == 1 {
		// Standalone expression — check if we have a compiled return type.
		if ct, ok := exprTypes[expr]; ok && ct != cel.DynType && ct != cel.AnyType {
			if dt := celTypeToDeclType(ct); dt != nil && dt != apiservercel.DynType {
				*narrowed = true
				return dt
			}
		}
		return apiservercel.DynType
	}
	return apiservercel.StringType
}

// celTypeToDeclType converts a cel.Type (from expression output) to an
// apiservercel.DeclType for use in the type provider. Only scalar and
// collection types are converted; structured object types remain dyn
// (the DeclType can't be reconstructed from the cel.Type alone).
func celTypeToDeclType(ct *cel.Type) *apiservercel.DeclType {
	switch {
	case ct == cel.StringType:
		return apiservercel.StringType
	case ct == cel.IntType:
		return apiservercel.IntType
	case ct == cel.BoolType:
		return apiservercel.BoolType
	case ct == cel.DoubleType:
		return apiservercel.DoubleType
	case ct == cel.BytesType:
		return apiservercel.BytesType
	case ct == cel.DurationType:
		return apiservercel.DurationType
	case ct == cel.TimestampType:
		return apiservercel.DateType
	case ct == cel.DynType || ct == cel.AnyType:
		return apiservercel.DynType
	default:
		// Check for list and map types via parameters.
		params := ct.Parameters()
		if len(params) == 1 {
			// list(T)
			elemDT := celTypeToDeclType(params[0])
			if elemDT != nil {
				return apiservercel.NewListType(elemDT, -1)
			}
		} else if len(params) == 2 {
			// map(K, V)
			keyDT := celTypeToDeclType(params[0])
			valDT := celTypeToDeclType(params[1])
			if keyDT != nil && valDT != nil {
				return apiservercel.NewMapType(keyDT, valDT, -1)
			}
		}
		// Structured types or unknown — keep as dyn.
		return apiservercel.DynType
	}
}
