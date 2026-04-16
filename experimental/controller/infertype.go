// infertype.go implements compile-time type inference for Graph node templates.
//
// Three type sources feed into the CEL environment:
//   - Resource schemas: OpenAPI schemas resolved from the API server for nodes
//     with literal apiVersion/kind.
//   - Definition types: structural inference from definition node templates.
//   - Fallback: dyn for nodes that cannot be typed.
//
// This file handles all three sources. It walks definition templates statically
// (no CEL compilation) and builds DeclType objects that the CEL type checker
// uses for field-level validation.
package graphcontroller

import (
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
	apiservercel "k8s.io/apiserver/pkg/cel"
	celopenapi "k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/kube-openapi/pkg/validation/spec"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
)

// typeSource holds all resolved type information for building the CEL environment.

// isWatchKindTemplate returns true when a template has a `selector` field,
// indicating it watches a collection. This is stricter than DetectReference's
// WatchKind classification (which uses "no metadata.name") because unnamed
// Own resources also lack metadata.name but are not collections. The selector
// field is the user-facing signal for "watch all instances of this kind" and
// is present on all WatchKind templates in practice.
func isWatchKindTemplate(tmpl map[string]any) bool {
	_, hasSelector := tmpl["selector"]
	return hasSelector
}

// Populated during compilation phases 1 (schema resolution) and 2 (definition inference).
type typeSource struct {
	// resourceSchemas maps node ID → OpenAPI schema for nodes with resolved GVKs.
	resourceSchemas map[string]*spec.Schema
	// definitionTypes maps node ID → inferred DeclType for definition nodes.
	definitionTypes map[string]*apiservercel.DeclType
	// forEachDefinitions tracks definition nodes that have forEach (scope is list, not object).
	forEachDefinitions map[string]bool
	// untypedIDs are node/variable identifiers declared as dyn.
	untypedIDs []string
	// listIDs are WatchKind node identifiers declared as list(dyn).
	// Comprehension macros (.map(), .filter(), .exists()) require list typing.
	listIDs []string
	// unresolvedGVKs are GVKs that had literal apiVersion/kind but whose schema
	// could not be resolved (CRD not yet installed). Used by the CRD watch to
	// detect when recompilation is needed.
	unresolvedGVKs []runtimeschema.GroupVersionKind
}

// resolveNodeTypes resolves types for all nodes in the spec.
// It resolves OpenAPI schemas for resource nodes and infers types for definition nodes.
// The resolver may be nil — all resource nodes fall back to dyn.
func resolveNodeTypes(nodes []Node, schemaResolver resolver.SchemaResolver) *typeSource {
	ts := &typeSource{
		resourceSchemas:    make(map[string]*spec.Schema),
		definitionTypes:    make(map[string]*apiservercel.DeclType),
		forEachDefinitions: make(map[string]bool),
	}

	// Track all identifiers that need CEL declarations.
	seen := make(map[string]bool)

	for _, node := range nodes {
		seen[node.ID] = true

		ref := node.Reference()
		switch {
		case ref == ReferenceDefinition:
			// Phase 2: infer type from template structure.
			typeName := krocel.TypeNamePrefix + node.ID
			dt := inferObjectType(typeName, node.Template)
			ts.definitionTypes[node.ID] = dt
			if node.ForEach != nil {
				ts.forEachDefinitions[node.ID] = true
			}

		case schemaResolver != nil && ref != ReferenceDefinition:
			// Phase 1: resolve schema for resource nodes with literal GVK.
			gvk := extractLiteralGVK(node.Template)
			if gvk != nil {
				s, err := schemaResolver.ResolveSchema(*gvk)
				if err == nil && s != nil {
					ts.resourceSchemas[node.ID] = s
					if isWatchKindTemplate(node.Template) {
						ts.listIDs = append(ts.listIDs, node.ID)
					}
					continue
				}
				// Resolution failed — track as unresolved, fall through to dyn.
				ts.unresolvedGVKs = append(ts.unresolvedGVKs, *gvk)
			}
			if isWatchKindTemplate(node.Template) {
				ts.listIDs = append(ts.listIDs, node.ID)
			} else {
				ts.untypedIDs = append(ts.untypedIDs, node.ID)
			}

		default:
			if isWatchKindTemplate(node.Template) {
				ts.listIDs = append(ts.listIDs, node.ID)
			} else {
				ts.untypedIDs = append(ts.untypedIDs, node.ID)
			}
		}

		// forEach iterator variables are always dyn.
		for varName := range node.ForEach {
			if !seen[varName] {
				seen[varName] = true
				ts.untypedIDs = append(ts.untypedIDs, varName)
			}
		}
	}

	return ts
}

// buildTypedEnvOptions constructs CEL environment options from resolved type sources.
// Builds a single DeclTypeProvider for both resource schemas and definition types,
// avoiding the double-provider problem where a second CustomTypeProvider replaces the first.
func buildTypedEnvOptions(ts *typeSource) []cel.EnvOption {
	var declarations []cel.EnvOption
	var allDeclTypes []*apiservercel.DeclType

	// Resource schemas → DeclTypes via SchemaDeclTypeWithMetadata.
	for id, s := range ts.resourceSchemas {
		declType := krocel.SchemaDeclTypeWithMetadata(&celopenapi.Schema{Schema: s}, false)
		if declType == nil {
			continue
		}
		typeName := krocel.TypeNamePrefix + id
		declType = declType.MaybeAssignTypeName(typeName)
		allDeclTypes = append(allDeclTypes, declType)
		declarations = append(declarations, cel.Variable(id, declType.CelType()))
	}

	// Definition types → DeclTypes from structural inference.
	for id, dt := range ts.definitionTypes {
		allDeclTypes = append(allDeclTypes, dt)
		celType := dt.CelType()
		if ts.forEachDefinitions[id] {
			// forEach definitions enter scope as list(elementType).
			declarations = append(declarations, cel.Variable(id, cel.ListType(celType)))
		} else {
			declarations = append(declarations, cel.Variable(id, celType))
		}
	}

	// Create a single DeclTypeProvider for all typed nodes.
	if len(allDeclTypes) > 0 {
		provider := krocel.NewDeclTypeProvider(allDeclTypes...)
		provider.SetRecognizeKeywordAsFieldName(true)
		registry := types.NewEmptyRegistry()
		wrappedProvider, err := provider.WithTypeProvider(registry)
		if err == nil {
			declarations = append(declarations, cel.CustomTypeProvider(wrappedProvider))
		}
	}

	return declarations
}

// ---------------------------------------------------------------------------
// Definition type inference (phase 2)
// ---------------------------------------------------------------------------

// inferObjectType builds a DeclType from a template map. Each key becomes a
// typed field. Nested maps produce nested ObjectTypes with path-based naming.
func inferObjectType(typeName string, tmpl map[string]any) *apiservercel.DeclType {
	fields := make(map[string]*apiservercel.DeclField, len(tmpl))
	for name, value := range tmpl {
		fieldPath := typeName + "." + name
		fieldType := inferFieldType(fieldPath, value)
		fields[name] = apiservercel.NewDeclField(name, fieldType, false, nil, nil)
	}
	return apiservercel.NewObjectType(typeName, fields)
}

// inferFieldType determines the CEL type of a template value.
func inferFieldType(path string, value any) *apiservercel.DeclType {
	switch v := value.(type) {
	case string:
		return inferStringType(v)
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
		return inferObjectType(path, v)
	case []any:
		if len(v) == 0 {
			return apiservercel.NewListType(apiservercel.DynType, -1)
		}
		elemType := inferFieldType(path+".@idx", v[0])
		return apiservercel.NewListType(elemType, -1)
	default:
		return apiservercel.DynType
	}
}

// inferStringType classifies a string value for type inference:
//   - Pure literal (no ${...}): string
//   - Standalone expression (${expr} is the entire string): dyn
//   - Embedded expression (text around ${expr}): string (interpolation always produces string)
func inferStringType(s string) *apiservercel.DeclType {
	dollars, _, start, end := findExpr(s, 0)
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

// ---------------------------------------------------------------------------
// Schema resolution helpers (phase 1)
// ---------------------------------------------------------------------------

// extractLiteralGVK extracts a GVK from a template if apiVersion and kind are
// literal strings (not CEL expressions). Returns nil if either is missing or
// contains an expression.
func extractLiteralGVK(tmpl map[string]any) *runtimeschema.GroupVersionKind {
	if tmpl == nil {
		return nil
	}
	apiVersion, ok := tmpl["apiVersion"].(string)
	if !ok || strings.Contains(apiVersion, "${") {
		return nil
	}
	kind, ok := tmpl["kind"].(string)
	if !ok || strings.Contains(kind, "${") {
		return nil
	}
	gv, err := runtimeschema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil
	}
	gvk := gv.WithKind(kind)
	return &gvk
}

// nodeTypeInfo returns a human-readable description of a node's type source.
// Used for logging and status reporting.
func nodeTypeInfo(ts *typeSource, nodeID string) string {
	if _, ok := ts.resourceSchemas[nodeID]; ok {
		return "schema"
	}
	if _, ok := ts.definitionTypes[nodeID]; ok {
		return "inferred"
	}
	return "dyn"
}

// countTypedNodes returns the number of nodes with resolved types (not dyn).
func countTypedNodes(ts *typeSource) (schemas, definitions, untyped int) {
	return len(ts.resourceSchemas), len(ts.definitionTypes), len(ts.untypedIDs)
}

// ---------------------------------------------------------------------------
// Formatting
// ---------------------------------------------------------------------------

func formatTypeSource(ts *typeSource) string {
	schemas, defs, untyped := countTypedNodes(ts)
	return fmt.Sprintf("schemas=%d definitions=%d untyped=%d", schemas, defs, untyped)
}
