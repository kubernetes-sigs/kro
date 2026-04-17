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
// Populated during compilation phases 1 (schema resolution) and 2 (definition inference).
type typeSource struct {
	// resourceSchemas maps node ID → OpenAPI schema for nodes with resolved GVKs.
	resourceSchemas map[string]*spec.Schema
	// resourceCollections marks resolved-schema node IDs whose CEL variable
	// should be typed as list(element) rather than the element itself
	// (Watch-class nodes expose a collection of observed objects).
	resourceCollections map[string]bool
	// definitionTypes maps node ID → inferred DeclType for definition nodes.
	definitionTypes map[string]*apiservercel.DeclType
	// forEachDefinitions tracks definition nodes that have forEach (scope is list, not object).
	forEachDefinitions map[string]bool
	// untypedIDs are node/variable identifiers declared as dyn.
	untypedIDs []string
	// listIDs are Watch node identifiers declared as list(dyn).
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
		resourceSchemas:     make(map[string]*spec.Schema),
		resourceCollections: make(map[string]bool),
		definitionTypes:     make(map[string]*apiservercel.DeclType),
		forEachDefinitions:  make(map[string]bool),
	}

	// Track all identifiers that need CEL declarations.
	seen := make(map[string]bool)

	for _, node := range nodes {
		seen[node.ID] = true

		ref := node.Type()
		switch {
		case ref == NodeTypeDef:
			// Phase 2: infer type from template structure.
			typeName := krocel.TypeNamePrefix + node.ID
			dt := inferObjectType(typeName, node.Payload())
			ts.definitionTypes[node.ID] = dt
			if node.ForEach != nil {
				ts.forEachDefinitions[node.ID] = true
			}

		case schemaResolver != nil && ref != NodeTypeDef:
			// Phase 1: resolve schema for resource nodes with literal GVK.
			//
			// forEach nodes skip schema resolution: the same variable
			// appears in two runtime contexts with incompatible shapes.
			// Inside per-item readyWhen, the coordinator swaps
			// scope[nodeID] to a single item map; sibling expressions
			// see the collection. A typed declaration breaks one of
			// the two access patterns. dyn accepts both.
			resolved := false
			if node.ForEach == nil {
				gvk := extractLiteralGVK(node.Identity())
				if gvk != nil && !isUpstreamKroGroup(gvk.Group) {
					s, err := schemaResolver.ResolveSchema(*gvk)
					if err == nil && s != nil {
						ts.resourceSchemas[node.ID] = s
						if ref == NodeTypeWatch {
							ts.resourceCollections[node.ID] = true
						}
						resolved = true
					} else {
						ts.unresolvedGVKs = append(ts.unresolvedGVKs, *gvk)
					}
				}
			}
			if !resolved {
				if ref == NodeTypeWatch {
					ts.listIDs = append(ts.listIDs, node.ID)
				} else {
					ts.untypedIDs = append(ts.untypedIDs, node.ID)
				}
			}

		default:
			if ref == NodeTypeWatch {
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
// resolution is skipped for this group because the API server may normalize
// the OpenAPI schema in ways that differ from the CRD YAML (e.g., stripping
// nested "metadata" properties). The experimental controller consumes but
// does not own these CRDs, and their stdlib templates (rgd.yaml) are
// authored against dyn semantics.
const upstreamKroAPIGroup = "kro.run"

// isUpstreamKroGroup reports whether a GVK group belongs to the upstream
// kro API. These CRDs skip schema resolution — see upstreamKroAPIGroup.
func isUpstreamKroGroup(group string) bool {
	return group == upstreamKroAPIGroup
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
	if !ok || isCELExpression(apiVersion) {
		return nil
	}
	kind, ok := tmpl["kind"].(string)
	if !ok || isCELExpression(kind) {
		return nil
	}
	gv, err := runtimeschema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil
	}
	gvk := gv.WithKind(kind)
	return &gvk
}
