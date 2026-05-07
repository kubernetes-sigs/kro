// typesource.go resolves OpenAPI schemas and builds CEL type declarations.
package compiler

import (
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
			resolveDefType(node, ts)
		case nodeType == graph.NodeTypeMetric:
			// Metric nodes don't publish to scope — declare as dyn for DAG participation.
			ts.UntypedIDs = append(ts.UntypedIDs, node.ID)
		case schemaResolver != nil:
			resolveResourceSchema(node, ts, schemaResolver)
		default:
			addUntyped(node, ts)
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

// resolveDefType handles def-node type inference (Phase 2).
func resolveDefType(node graph.Node, ts *TypeSource) {
	typeName := krocel.TypeNamePrefix + node.ID
	dt := InferObjectType(typeName, node.Payload())
	ts.DefinitionTypes[node.ID] = dt
	if node.ForEach != nil {
		ts.ForEachDefinitions[node.ID] = true
	}
}

// resolveResourceSchema handles non-def schema resolution (Phase 1).
// It resolves OpenAPI schemas from the API server, handling dynamic-GVK
// detection, forEach inner-scope schemas, and Watch-as-collection typing.
func resolveResourceSchema(node graph.Node, ts *TypeSource, schemaResolver resolver.SchemaResolver) {
	// forEach nodes skip schema resolution for the OUTER scope
	// (the node ID is dyn, permitting both field access and list ops).
	// The INNER scope (readyWhen on the forEach node itself) uses
	// element type — see cel.go Phase 4a-3.
	if node.ForEach != nil {
		resolveForEachInnerSchema(node, ts, schemaResolver)
		addUntyped(node, ts)
		return
	}

	if resolveStaticGVK(node, ts, schemaResolver) {
		return
	}

	// Not resolved — check for dynamic GVK.
	gvk := ExtractLiteralGVK(node.Identity())
	if gvk == nil && node.HasDynamicGVR() {
		// Dynamic GVK node — type depends on a CEL expression evaluated at
		// runtime. The caller may pre-populate the schema after resolution
		// via TypeSource.PrePopulateSchema.
		ts.DynamicGVKNodes = append(ts.DynamicGVKNodes, node.ID)
	}

	addUntyped(node, ts)
}

// resolveStaticGVK attempts to resolve the schema for a non-forEach node with
// a literal GVK. Returns true if the schema was resolved and the node is typed.
func resolveStaticGVK(node graph.Node, ts *TypeSource, schemaResolver resolver.SchemaResolver) bool {
	gvk := ExtractLiteralGVK(node.Identity())
	if gvk == nil || isUpstreamKroInfra(gvk) {
		return false
	}
	s, err := schemaResolver.ResolveSchema(*gvk)
	if err != nil || s == nil {
		ts.UnresolvedGVKs = append(ts.UnresolvedGVKs, *gvk)
		return false
	}
	ts.ResourceSchemas[node.ID] = s
	if node.Type() == graph.NodeTypeWatch {
		ts.resourceCollections[node.ID] = true
	}
	return true
}

// resolveForEachInnerSchema resolves the schema for a forEach node's inner
// scope (used for readyWhen validation in Phase 4a-3). The schema is stored
// in forEachSchemas, not in the typed declarations.
func resolveForEachInnerSchema(node graph.Node, ts *TypeSource, schemaResolver resolver.SchemaResolver) {
	gvk := ExtractLiteralGVK(node.Identity())
	if gvk == nil || isUpstreamKroInfra(gvk) {
		return
	}
	s, err := schemaResolver.ResolveSchema(*gvk)
	if err == nil && s != nil {
		ts.forEachSchemas[node.ID] = s
	}
}

// addUntyped adds a node to the appropriate untyped list based on its type.
// Watch nodes become list(dyn); all others become dyn.
func addUntyped(node graph.Node, ts *TypeSource) {
	if node.Type() == graph.NodeTypeWatch {
		// Watch nodes are list(dyn) to support comprehension macros.
		ts.listIDs = append(ts.listIDs, node.ID)
	} else {
		// Other nodes stay as dyn (permissive for both field access and list ops).
		ts.UntypedIDs = append(ts.UntypedIDs, node.ID)
	}
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
// declared types — field-name checking on e.g. k.spec.schema.kind still catches
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
	"Graph":                   true,
	"GraphRevision":           true,
	"ResourceGraphDefinition": true,
}

// isUpstreamKroInfra reports whether a GVK belongs to the upstream kro
// infrastructure (not user-defined CRDs that happen to use the kro.run group).
func isUpstreamKroInfra(gvk *runtimeschema.GroupVersionKind) bool {
	return gvk.Group == upstreamKroAPIGroup && kroInfraKinds[gvk.Kind]
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
