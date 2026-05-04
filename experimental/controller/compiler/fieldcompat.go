// fieldcompat.go validates that CEL expression return types match schema field types.
package compiler

import (
	"fmt"

	"github.com/google/cel-go/cel"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/ellistarn/kro/experimental/controller/graph"
)

// validateExprFieldCompat checks that standalone expressions in node bodies
// produce types compatible with the destination field's schema. For example,
// spec.replicas: ${someString} is rejected if the schema says replicas is
// integer. Only checked for nodes with resolved schemas (template, patch, ref).
func validateExprFieldCompat(nodes []graph.Node, ts *TypeSource, exprTypes map[string]*cel.Type) error {
	for _, node := range nodes {
		nodeType := node.Type()
		if nodeType == graph.NodeTypeDef {
			continue // defs don't have external schemas
		}
		s, ok := ts.ResourceSchemas[node.ID]
		if !ok || s == nil {
			continue // no schema resolved — can't check
		}
		body := node.Body()
		if body == nil {
			continue
		}
		if err := checkFieldCompat(node.ID, body, s, exprTypes, ""); err != nil {
			return err
		}
	}
	return nil
}

// checkFieldCompat recursively walks a body map and its corresponding schema,
// checking that standalone expression return types are compatible with schema
// field types.
func checkFieldCompat(nodeID string, body map[string]any, s *spec.Schema, exprTypes map[string]*cel.Type, path string) error {
	if s == nil || s.Properties == nil {
		return nil
	}
	for key, value := range body {
		fieldPath := path + "." + key
		fieldSchema, ok := s.Properties[key]
		if !ok {
			continue // field not in schema — skip (extra fields allowed)
		}

		switch v := value.(type) {
		case string:
			dollars, expr, start, end := graph.FindExpr(v, 0)
			if start < 0 || start != 0 || end != len(v) || len(dollars) != 1 {
				continue // not a standalone expression
			}
			ct, ok := exprTypes[expr]
			if !ok || ct == cel.DynType || ct == cel.AnyType {
				continue // unknown type — can't check
			}
			if err := checkTypeCompat(nodeID, fieldPath, expr, ct, &fieldSchema); err != nil {
				return err
			}

		case map[string]any:
			if err := checkFieldCompat(nodeID, v, &fieldSchema, exprTypes, fieldPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkTypeCompat validates that a CEL return type is compatible with an OpenAPI
// schema type. Returns an error if the types are incompatible.
func checkTypeCompat(nodeID, fieldPath, expr string, celType *cel.Type, fieldSchema *spec.Schema) error {
	schemaType := fieldSchema.Type
	if len(schemaType) == 0 {
		return nil // no type constraint in schema
	}
	expectedType := schemaType[0]

	// Map CEL types to OpenAPI type names.
	var celTypeName string
	switch {
	case celType == cel.StringType:
		celTypeName = "string"
	case celType == cel.IntType:
		celTypeName = "integer"
	case celType == cel.BoolType:
		celTypeName = "boolean"
	case celType == cel.DoubleType:
		celTypeName = "number"
	default:
		// Complex types (list, map, object) — skip for now.
		return nil
	}

	if celTypeName != expectedType {
		return fmt.Errorf("node %q: expression %q at %s returns %s, but schema expects %s: %w",
			nodeID, expr, fieldPath, celTypeName, expectedType, ErrInvalidExpression)
	}
	return nil
}
