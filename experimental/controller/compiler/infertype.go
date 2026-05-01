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
package compiler

import (
	"fmt"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
	apiservercel "k8s.io/apiserver/pkg/cel"
	celopenapi "k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/ellistarn/kro/experimental/controller/graph"
	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
)

// TypeSource holds all resolved type information for building the CEL environment.
// Populated during compilation phases 1 (schema resolution) and 2 (definition inference).
type TypeSource struct {
	// ResourceSchemas maps node ID → OpenAPI schema for nodes with resolved GVKs.
	// Does NOT include forEach nodes (those stay dyn in outer scope).
	ResourceSchemas map[string]*spec.Schema
	// forEachSchemas maps forEach node ID → OpenAPI schema, used only for
	// inner-scope readyWhen validation (Phase 4a-3 in cel.go). These nodes
	// are NOT added to typed declarations — they stay dyn in outer scope.
	forEachSchemas map[string]*spec.Schema
	// resourceCollections marks resolved-schema node IDs whose CEL variable
	// should be typed as list(element) rather than the element itself
	// (Watch-class nodes expose a collection of observed objects).
	resourceCollections map[string]bool
	// DefinitionTypes maps node ID → inferred DeclType for definition nodes.
	DefinitionTypes map[string]*apiservercel.DeclType
	// ForEachDefinitions tracks definition nodes that have forEach (scope is list, not object).
	ForEachDefinitions map[string]bool
	// UntypedIDs are node/variable identifiers declared as dyn.
	UntypedIDs []string
	// listIDs are Watch node identifiers declared as list(dyn).
	// Comprehension macros (.map(), .filter(), .exists()) require list typing.
	listIDs []string
	// unresolvedGVKs are GVKs that had literal apiVersion/kind but whose schema
	// could not be resolved (CRD not yet installed). Used by the CRD watch to
	// detect when recompilation is needed.
	UnresolvedGVKs []runtimeschema.GroupVersionKind
	// narrowedIterators maps forEach iterator variable names to their element
	// cel.Type, narrowed from the collection expression's return type during
	// the type refinement pass. These variables are declared as the element
	// type in the refined environment instead of dyn.
	narrowedIterators map[string]*cel.Type
	// DynamicGVKNodes lists node IDs whose apiVersion or kind contains a CEL
	// expression. Per 004-compilation.md § Deferred Types: the type is
	// unknowable until runtime.
	DynamicGVKNodes []string
}

// PrePopulateSchema injects a resolved schema for a dynamic GVK node. Called
// by the compilation caller (not the compiler) when the node's GVK was resolved
// on a previous reconcile. The compiler sees the pre-populated schema and types
// the node like any static resource node.
//
// Per 004-compilation.md § Deferred Types: "The caller resolves the schema for
// the recorded GVK and pre-populates the type source before calling the compiler."
func (ts *TypeSource) PrePopulateSchema(nodeID string, gvk runtimeschema.GroupVersionKind, schemaResolver resolver.SchemaResolver) {
	s, err := schemaResolver.ResolveSchema(gvk)
	if err != nil || s == nil {
		return // schema not available — node stays dyn
	}
	ts.ResourceSchemas[nodeID] = s
	// Remove from untypedIDs so the CEL environment doesn't declare both
	// a typed and an untyped binding for the same node.
	filtered := ts.UntypedIDs[:0]
	for _, id := range ts.UntypedIDs {
		if id != nodeID {
			filtered = append(filtered, id)
		}
	}
	ts.UntypedIDs = filtered
}

// ResolveNodeTypes resolves types for all nodes in the spec.
// It resolves OpenAPI schemas for resource nodes and infers types for definition nodes.
// The resolver may be nil — all resource nodes fall back to dyn.
func ResolveNodeTypes(nodes []graph.Node, schemaResolver resolver.SchemaResolver) *TypeSource {
	ts := &TypeSource{
		ResourceSchemas:     make(map[string]*spec.Schema),
		forEachSchemas:      make(map[string]*spec.Schema),
		resourceCollections: make(map[string]bool),
		DefinitionTypes:     make(map[string]*apiservercel.DeclType),
		ForEachDefinitions:  make(map[string]bool),
	}

	// Track all identifiers that need CEL declarations.
	seen := make(map[string]bool)

	for _, node := range nodes {
		seen[node.ID] = true

		nodeType := node.Type()
		switch {
		case nodeType == graph.NodeTypeDef:
			// Phase 2: infer type from template structure.
			typeName := krocel.TypeNamePrefix + node.ID
			dt := InferObjectType(typeName, node.Payload())
			ts.DefinitionTypes[node.ID] = dt
			if node.ForEach != nil {
				ts.ForEachDefinitions[node.ID] = true
			}

		case schemaResolver != nil && nodeType != graph.NodeTypeDef:
			// Phase 1: resolve schema for resource nodes with literal GVK.
			//
			// forEach nodes skip schema resolution for the OUTER scope
			// (the node ID is dyn, permitting both field access and list ops).
			// The INNER scope (readyWhen on the forEach node itself) uses
			// element type — see cel.go Phase 4a-3.
			resolved := false
			if node.ForEach == nil {
				gvk := ExtractLiteralGVK(node.Identity())
				if gvk != nil && !isUpstreamKroInfra(gvk) {
					s, err := schemaResolver.ResolveSchema(*gvk)
					if err == nil && s != nil {
						ts.ResourceSchemas[node.ID] = s
						if nodeType == graph.NodeTypeWatch {
							ts.resourceCollections[node.ID] = true
						}
						resolved = true
					} else {
						ts.UnresolvedGVKs = append(ts.UnresolvedGVKs, *gvk)
					}
				} else if gvk == nil && node.HasDynamicGVR() {
					// Dynamic GVK node — type depends on a CEL expression
					// evaluated at runtime. Record for tracking. The caller
					// may pre-populate the schema after resolution via
					// TypeSource.PrePopulateSchema.
					ts.DynamicGVKNodes = append(ts.DynamicGVKNodes, node.ID)
				}
			} else {
				// forEach nodes: resolve schema for inner-scope readyWhen
				// validation but DON'T add to typed declarations. Store
				// in forEachSchemas for the Phase 4a-3 inner-scope check.
				gvk := ExtractLiteralGVK(node.Identity())
				if gvk != nil && !isUpstreamKroInfra(gvk) {
					s, err := schemaResolver.ResolveSchema(*gvk)
					if err == nil && s != nil {
						ts.forEachSchemas[node.ID] = s
					}
				}
			}
		if !resolved {
			if nodeType == graph.NodeTypeWatch {
					// Unresolved Watch nodes are list(dyn) to support
					// comprehension macros (.map, .filter, .exists).
					ts.listIDs = append(ts.listIDs, node.ID)
				} else {
					// Unresolved forEach nodes stay as dyn (not list(dyn)).
					// dyn is permissive for both field access and list ops.
					// list(dyn) breaks field access: ${instances.status}
					// fails because lists don't have .status.
					ts.UntypedIDs = append(ts.UntypedIDs, node.ID)
				}
			}

		default:
			if nodeType == graph.NodeTypeWatch {
				ts.listIDs = append(ts.listIDs, node.ID)
			} else {
				ts.UntypedIDs = append(ts.UntypedIDs, node.ID)
			}
		}

		// forEach iterator variables are always dyn.
		if node.ForEach != nil {
			varName := node.ForEach.VarName
			if !seen[varName] {
				seen[varName] = true
				ts.UntypedIDs = append(ts.UntypedIDs, varName)
			}
		}
	}

	return ts
}

// buildTypedEnvOptions constructs CEL environment options from resolved type sources.
// Builds a single DeclTypeProvider for both resource schemas and definition types,
// avoiding the double-provider problem where a second CustomTypeProvider replaces the first.
func buildTypedEnvOptions(ts *TypeSource) []cel.EnvOption {
	var declarations []cel.EnvOption
	var allDeclTypes []*apiservercel.DeclType

	// Resource schemas → DeclTypes via SchemaDeclTypeWithMetadata.
	for id, s := range ts.ResourceSchemas {
		declType := krocel.SchemaDeclTypeWithMetadata(&celopenapi.Schema{Schema: s}, false)
		if declType == nil {
			continue
		}
		// Post-process: loosen list fields whose items have
		// x-kubernetes-preserve-unknown-fields in the raw OpenAPI schema.
		// These lists contain partially-opaque items that can't be used
		// in typed list concat. We check the SCHEMA, not the DeclType,
		// because SchemaDeclTypeWithMetadata may elide metadata for
		// schemas without an explicit type declaration (e.g. simpleSchema
		// `any` produces preserve-unknown-fields with no `type: object`).
		declType = loosenOpaqueFields(declType, s)
		typeName := krocel.TypeNamePrefix + id
		declType = declType.MaybeAssignTypeName(typeName)
		allDeclTypes = append(allDeclTypes, declType)
		celType := declType.CelType()
		if ts.resourceCollections[id] {
			celType = cel.ListType(celType)
		}
		declarations = append(declarations, cel.Variable(id, celType))
	}

	// Definition types → DeclTypes from structural inference.
	for id, dt := range ts.DefinitionTypes {
		allDeclTypes = append(allDeclTypes, dt)
		celType := dt.CelType()
		if ts.ForEachDefinitions[id] {
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

	// Narrowed forEach iterators (from the type refinement pass).
	// These are declared with their element type instead of dyn.
	for varName, elemCelType := range ts.narrowedIterators {
		declarations = append(declarations, cel.Variable(varName, elemCelType))
	}

	return declarations
}

// loosenOpaqueFields walks a DeclType tree and replaces list-typed
// fields whose items have x-kubernetes-preserve-unknown-fields in the
// raw OpenAPI schema with DynType. This enables typed list operations
// (concat, map, filter) to work when the list items are partially-opaque.
//
// The standard CEL list concat overload requires list(A) + list(A) with
// the SAME type parameter A. When one operand is a map literal
// (list(map(string,dyn))) and the other is a typed list of named structs
// (list(Node)), the type checker cannot unify A. Declaring such lists as
// dyn allows concat to work via CEL's permissive dyn handling.
//
// This is targeted: only list fields whose items carry preserve-unknown
// in the raw schema get loosened. Scalar and object fields retain their
// declared types — field-name checking on e.g. k.spec.kind still catches
// typos.
func loosenOpaqueFields(dt *apiservercel.DeclType, schema *spec.Schema) *apiservercel.DeclType {
	if dt == nil || !dt.IsObject() || schema == nil {
		return dt
	}
	newFields := make(map[string]*apiservercel.DeclField, len(dt.Fields))
	changed := false
	for name, field := range dt.Fields {
		ft := field.Type
		propSchema := schemaProperty(schema, name)
		switch {
		case ft.IsList() && propSchema != nil && schemaItemsHavePreserveUnknown(propSchema):
			// List of partially-opaque items → dyn.
			newFields[name] = apiservercel.NewDeclField(
				name, apiservercel.DynType, field.Required, nil, nil,
			)
			changed = true
		case ft.IsObject() && propSchema != nil && schemaIsSelfPreserveUnknown(propSchema):
			// Object field declared with x-kubernetes-preserve-unknown-fields.
			// These fields are intentionally opaque — their content shape
			// is not statically known. Declaring them as dyn allows
			// ternary expressions, map operations, and .merge() to work
			// without type mismatches against map literals.
			newFields[name] = apiservercel.NewDeclField(
				name, apiservercel.DynType, field.Required, nil, nil,
			)
			changed = true
		case ft.IsObject() && propSchema != nil:
			loosened := loosenOpaqueFields(ft, propSchema)
			if loosened != ft {
				newFields[name] = apiservercel.NewDeclField(
					name, loosened, field.Required, nil, nil,
				)
				changed = true
			} else {
				newFields[name] = field
			}
		default:
			newFields[name] = field
		}
	}
	if !changed {
		return dt
	}
	result := apiservercel.NewObjectType(dt.TypeName(), newFields)
	// Preserve metadata from the original type. NewObjectType does not
	// copy metadata, but MaybeAssignTypeName does. If the original type
	// carried x-kubernetes-preserve-unknown-fields metadata (e.g., the
	// spec object itself allows unknown fields), we must not lose it.
	if dt.Metadata != nil {
		result.Metadata = make(map[string]string, len(dt.Metadata))
		for k, v := range dt.Metadata {
			result.Metadata[k] = v
		}
	}
	return result
}

// schemaProperty returns the sub-schema for a named property, or nil.
func schemaProperty(schema *spec.Schema, name string) *spec.Schema {
	if schema == nil {
		return nil
	}
	if p, ok := schema.Properties[name]; ok {
		return &p
	}
	return nil
}

// schemaIsSelfPreserveUnknown checks whether the schema itself (not children)
// declares x-kubernetes-preserve-unknown-fields.
func schemaIsSelfPreserveUnknown(schema *spec.Schema) bool {
	if schema == nil || schema.VendorExtensible.Extensions == nil {
		return false
	}
	v, ok := schema.VendorExtensible.Extensions.GetBool("x-kubernetes-preserve-unknown-fields")
	return ok && v
}

// schemaItemsHavePreserveUnknown checks whether an array schema's
// items carry x-kubernetes-preserve-unknown-fields anywhere in their tree.
func schemaItemsHavePreserveUnknown(schema *spec.Schema) bool {
	if schema == nil || schema.Items == nil || schema.Items.Schema == nil {
		return false
	}
	return schemaHasPreserveUnknown(schema.Items.Schema)
}

// schemaHasPreserveUnknown reports whether the schema declares
// x-kubernetes-preserve-unknown-fields anywhere in its tree.
func schemaHasPreserveUnknown(s *spec.Schema) bool {
	if s == nil {
		return false
	}
	if s.VendorExtensible.Extensions != nil {
		if v, ok := s.VendorExtensible.Extensions.GetBool("x-kubernetes-preserve-unknown-fields"); ok && v {
			return true
		}
	}
	for i := range s.Properties {
		p := s.Properties[i]
		if schemaHasPreserveUnknown(&p) {
			return true
		}
	}
	if s.Items != nil && s.Items.Schema != nil {
		if schemaHasPreserveUnknown(s.Items.Schema) {
			return true
		}
	}
	if s.AdditionalProperties != nil && s.AdditionalProperties.Schema != nil {
		if schemaHasPreserveUnknown(s.AdditionalProperties.Schema) {
			return true
		}
	}
	return false
}

// upstreamKroAPIGroup is the API group for upstream kro CRDs. Schema
// resolution is skipped for infrastructure CRDs in this group because the
// API server may normalize the OpenAPI schema in ways that differ from the
// CRD YAML (e.g., stripping nested "metadata" properties). These CRDs use
// x-kubernetes-preserve-unknown-fields, making their resolved schemas
// misleadingly untyped.
//
// User-defined CRDs created by the RGD system may also use this group, but
// they have proper OpenAPI schemas generated from simple-schema declarations.
// Only the infrastructure kinds are excluded.
const upstreamKroAPIGroup = "kro.run"

// kroInfraKinds are the infrastructure CRD kinds in the kro.run group that
// skip schema resolution. User-defined CRDs (created by the RGD system)
// with other kinds in the same group get normal schema resolution.
var kroInfraKinds = map[string]bool{
	"Graph":                     true,
	"GraphRevision":             true,
	"ResourceGraphDefinition":   true,
}

// isUpstreamKroInfra reports whether a GVK belongs to the upstream kro
// infrastructure (not user-defined CRDs that happen to use the kro.run group).
func isUpstreamKroInfra(gvk *runtimeschema.GroupVersionKind) bool {
	return gvk.Group == upstreamKroAPIGroup && kroInfraKinds[gvk.Kind]
}

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

// ---------------------------------------------------------------------------
// Type refinement (second pass)
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Schema resolution helpers (phase 1)
// ---------------------------------------------------------------------------

// ExtractLiteralGVK extracts a GVK from a template if apiVersion and kind are
// literal strings (not CEL expressions). Returns nil if either is missing or
// contains an expression.
func ExtractLiteralGVK(tmpl map[string]any) *runtimeschema.GroupVersionKind {
	if tmpl == nil {
		return nil
	}
	apiVersion, ok := tmpl["apiVersion"].(string)
	if !ok || graph.IsCELExpression(apiVersion) {
		return nil
	}
	kind, ok := tmpl["kind"].(string)
	if !ok || graph.IsCELExpression(kind) {
		return nil
	}
	gv, err := runtimeschema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil
	}
	gvk := gv.WithKind(kind)
	return &gvk
}

// ---------------------------------------------------------------------------
// Expression-to-field compatibility
// ---------------------------------------------------------------------------

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
