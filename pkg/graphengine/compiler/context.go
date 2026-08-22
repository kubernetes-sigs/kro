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

package compiler

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/kube-openapi/pkg/validation/spec"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph/parser"
	"github.com/kubernetes-sigs/kro/pkg/graph/schema"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

// CompilationContext is the ambient environment for compiling one frame of a
// Graph. It carries two independent axes:
//
//   - GVK axis: schema resolution + REST mapping for a node's target type,
//     and the static-vs-dynamic-GVK decision. The resolver, REST mapper, and
//     per-compile field cache are shared (same pointer) across every frame of
//     a single Compile — a node's GVK is resolved the same way no matter how
//     deeply it is nested.
//   - Scope axis: the lexical frame chain. parent links to the enclosing
//     frame; name resolution walks outward (nearest-wins = shadowing). A root
//     context has parent == nil.
//
// The two axes are orthogonal: a dynamic-GVK node can live at any frame, and a
// frame can be entirely static. Nested compilation (subgraph nodes) pushes a
// child context per frame; the parent chain is the spine name resolution walks.
type CompilationContext struct {
	// parent is the enclosing lexical frame, or nil at the root.
	parent *CompilationContext

	// GVK axis — shared infra, identical pointer in every frame of a Compile.
	schemaResolver resolver.SchemaResolver
	restMapper     meta.RESTMapper
	fieldCache     *schema.Cache

	// localIDs is the set of node IDs declared at THIS frame. Populated up
	// front (before node building) so a child frame compiled mid-build can
	// still resolve forward references to this frame's later nodes.
	localIDs map[string]struct{}

	// nodeSchemaOverrides declares per-node publication schemas supplied by
	// the caller via WithNodeSchemaOverride. A Def node with an override
	// publishes that schema instead of the value-shape inference of
	// inferDefSchema — used by the RGD adapter to type the `schema` node
	// with the RGD's declared SimpleSchema, which stays stable across
	// reconciles even when the instance is missing fields.
	nodeSchemaOverrides map[string]*spec.Schema

	// literalNodes declares nodes whose payloads are treated as literal data
	// (no CEL expression parsing). Used for instance seed nodes.
	literalNodes map[string]struct{}

	// softDepNodes and dataPendingTolerant carry the per-node affordances set
	// via WithSoftDependencies / WithDataPendingTolerant. They flow to child
	// frames unchanged; the RGD adapter only uses them for the root-frame
	// author-status writeback node, but sharing keeps the semantics uniform.
	softDepNodes        map[string]struct{}
	dataPendingTolerant map[string]struct{}

	costLimit uint64
}

// newRootContext builds the top-level compilation context for a single
// Compile. The field cache is freshly allocated so lookups don't leak across
// independent compiles; the resolver and REST mapper are shared from the
// owning Compiler.
func newRootContext(sr resolver.SchemaResolver, rm meta.RESTMapper) *CompilationContext {
	return &CompilationContext{
		schemaResolver:      sr,
		restMapper:          rm,
		fieldCache:          schema.NewCache(),
		localIDs:            map[string]struct{}{},
		nodeSchemaOverrides: map[string]*spec.Schema{},
		literalNodes:        map[string]struct{}{},
	}
}

// child pushes a nested lexical frame. The GVK-axis infra (resolver, REST
// mapper, field cache) is shared with the parent — a node's GVK resolves the
// same way at any depth; only the frame state is fresh. parent links back so
// name resolution can walk outward.
func (ctx *CompilationContext) child() *CompilationContext {
	return &CompilationContext{
		parent:              ctx,
		schemaResolver:      ctx.schemaResolver,
		restMapper:          ctx.restMapper,
		fieldCache:          ctx.fieldCache,
		localIDs:            map[string]struct{}{},
		nodeSchemaOverrides: ctx.nodeSchemaOverrides,
		literalNodes:        ctx.literalNodes,
		softDepNodes:        ctx.softDepNodes,
		dataPendingTolerant: ctx.dataPendingTolerant,
		costLimit:           ctx.costLimit,
	}
}

// frameDepth reports how many frames up id is declared: 0 if local to this
// frame, 1 for the immediate parent, and so on; -1 if id is not declared in
// any enclosing frame. Nearest-frame-wins is exactly lexical shadowing — a
// child node sharing a parent node's name resolves to depth 0.
func (ctx *CompilationContext) frameDepth(id string) int {
	for d, c := 0, ctx; c != nil; d, c = d+1, c.parent {
		if _, ok := c.localIDs[id]; ok {
			return d
		}
	}
	return -1
}

// ancestorIDs returns every node ID visible from an enclosing frame that is
// not shadowed by a local declaration. These are declared as dyn identifiers
// in this frame's typed env so captured (cross-frame) references type-check —
// the frame boundary is the dynamic seam, while within-frame references keep
// full type checking.
func (ctx *CompilationContext) ancestorIDs() []string {
	seen := make(map[string]struct{}, len(ctx.localIDs))
	for id := range ctx.localIDs {
		seen[id] = struct{}{} // a local name shadows the same name in any ancestor
	}
	var ids []string
	for c := ctx.parent; c != nil; c = c.parent {
		for id := range c.localIDs {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	return ids
}

// buildNode produces a single compiled Node from its API form, parses CEL
// fragments out of the payload, and returns the OpenAPI schema the node
// publishes to scope (nil for Def and dynamic-GVK templates).
func (ctx *CompilationContext) buildNode(p *parser.Parser, n *expv1alpha1.Node, order int) (*Node, *spec.Schema, error) {
	kind, payload, err := projectPayload(n)
	if err != nil {
		return nil, nil, err
	}

	// Def nodes have no target GVK, but we still infer an OpenAPI
	// schema from the literal payload so the typed CEL env can narrow
	// def-sourced expressions. Fields whose literal value is a CEL
	// fragment (e.g. `${other.x}`) stay dyn — see inferDefSchema.
	// Literal def nodes (e.g. instance schema nodes) skip expression parsing.
	if kind == NodeKindDef {
		var descriptors []variable.FieldDescriptor
		if _, isLiteral := ctx.literalNodes[n.ID]; !isLiteral {
			var err error
			descriptors, _, err = parser.ParseSchemalessResource(payload)
			if err != nil {
				return nil, nil, fmt.Errorf("parse def payload: %w", err)
			}
		}
		vars, forEach, includeWhen, readyWhen, err := parseNodeCommon(n, descriptors)
		if err != nil {
			return nil, nil, err
		}
		node := &Node{
			ID:          n.ID,
			Index:       order,
			Kind:        kind,
			Object:      &unstructured.Unstructured{Object: payload},
			Variables:   vars,
			ForEach:     forEach,
			IncludeWhen: includeWhen,
			ReadyWhen:   readyWhen,
		}
		if override, ok := ctx.nodeSchemaOverrides[n.ID]; ok {
			return node, override, nil
		}
		return node, inferDefSchema(payload), nil
	}

	// A Template or Patch whose apiVersion or kind is a CEL expression has
	// no compile-time GVK: we can't resolve a schema, a REST mapping, or
	// type-check the payload against a target shape. Parse it schemaless,
	// flag it dynamic, and let the executor resolve the concrete GVK
	// per rendered object at apply time. Cross-node references inside the
	// payload are still type-checked later against the typed env; only the
	// node's own field types fall back to dyn.
	if (kind == NodeKindTemplate || kind == NodeKindPatch) && isDynamicGVK(payload) {
		return ctx.buildDynamicNode(n, order, payload, kind)
	}

	// Template/Ref/Patch all target a real GVK. Resolve schema and
	// REST mapping, then parse the payload for CEL fragments.
	// Templates and patches are authored manifests so we enforce
	// metadata-shape strictly. Ref payloads are synthesized from typed
	// structs that don't carry a metadata field — apiVersion + kind are
	// still required.
	authoredManifest := kind == NodeKindTemplate || kind == NodeKindPatch
	if err := validateKubernetesObjectStructure(payload, authoredManifest); err != nil {
		return nil, nil, err
	}
	gvk, err := extractGVKFromUnstructured(payload)
	if err != nil {
		return nil, nil, err
	}
	sch, err := ctx.schemaResolver.ResolveSchema(gvk)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve schema for %s: %w", gvk, err)
	}
	mapping, err := ctx.restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, nil, fmt.Errorf("REST mapping for %s: %w", gvk, err)
	}
	if mapping.Scope.Name() != meta.RESTScopeNameNamespace {
		// Cluster-scoped targets must not carry a namespace; otherwise the
		// SSA apply silently lands in the wrong shape and the user has no
		// idea why their resource didn't reach the cluster.
		if ns := nestedString(payload, "metadata", "namespace"); ns != "" {
			return nil, nil, fmt.Errorf("%s is cluster-scoped but template sets metadata.namespace=%q", gvk.Kind, ns)
		}
	}

	var descriptors []variable.FieldDescriptor
	if kind == NodeKindTemplate || kind == NodeKindPatch {
		if kind == NodeKindTemplate && gvk.Group == "apiextensions.k8s.io" && gvk.Kind == "CustomResourceDefinition" {
			descriptors, _, err = parser.ParseSchemalessResource(payload)
			if err != nil {
				return nil, nil, fmt.Errorf("parse %s payload: %w", kind, err)
			}
			for _, expr := range descriptors {
				if !strings.HasPrefix(expr.Path, "metadata.") {
					return nil, nil, fmt.Errorf("CEL expressions in CRDs are only supported for metadata fields, found in path %q, resource %s", expr.Path, n.ID)
				}
			}
		} else {
			// A patch body is a partial manifest shaped like the target, so it
			// type-checks against the target schema exactly as a template does.
			descriptors, err = p.ParseResource(payload, sch)
		}
	} else {
		// Ref payloads are synthesized from typed structs (ExternalRef).
		// The OpenAPI schema for the target GVK does not match
		// that shape — parse schemaless instead.
		descriptors, _, err = parser.ParseSchemalessResource(payload)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s payload: %w", kind, err)
	}

	vars, forEach, includeWhen, readyWhen, err := parseNodeCommon(n, descriptors)
	if err != nil {
		return nil, nil, err
	}

	return &Node{
		ID:          n.ID,
		Index:       order,
		Kind:        kind,
		GVR:         mapping.Resource,
		Namespaced:  mapping.Scope.Name() == meta.RESTScopeNameNamespace,
		Subresource: patchSubresource(n),
		Object:      &unstructured.Unstructured{Object: payload},
		Variables:   vars,
		ForEach:     forEach,
		IncludeWhen: includeWhen,
		ReadyWhen:   readyWhen,
		// A Ref whose payload carries metadata.selector is a read-only
		// collection of external objects (list-by-selector), so it publishes
		// a list into scope like a forEach collection.
		Collection: kind == NodeKindRef && hasMetadataSelector(payload),
	}, sch, nil
}

func parseNodeCommon(n *expv1alpha1.Node, descriptors []variable.FieldDescriptor) ([]*variable.ResourceField, []ForEachDimension, []*krocel.Expression, []*krocel.Expression, error) {
	forEach, err := parseForEachDimensions(n.ForEach)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	includeWhen, readyWhen, err := parseConditions(n)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return fieldDescriptorsToVariables(descriptors), forEach, includeWhen, readyWhen, nil
}

// patchSubresource returns the target subresource for a patch node and ""
// for every other kind.
func patchSubresource(n *expv1alpha1.Node) string {
	if n.Patch != nil {
		return n.Patch.Subresource
	}
	return ""
}

// buildDynamicNode compiles a Template or Patch whose apiVersion or kind is
// a CEL expression. There is no compile-time GVK, so we skip schema
// resolution and REST mapping, parse the payload schemaless, and mark the
// node dynamic. metadata is still required (payloads are authored) and
// apiVersion/kind must be non-empty strings, but the version segment isn't
// validated — it isn't a literal version yet. The node publishes no schema
// (nil), so downstream references see it as dyn until the executor pins the
// GVK.
func (ctx *CompilationContext) buildDynamicNode(n *expv1alpha1.Node, order int, payload map[string]interface{}, kind NodeKind) (*Node, *spec.Schema, error) {
	if err := validateDynamicTemplateStructure(payload); err != nil {
		return nil, nil, err
	}
	descriptors, _, err := parser.ParseSchemalessResource(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("parse dynamic %s payload: %w", kind, err)
	}
	vars, forEach, includeWhen, readyWhen, err := parseNodeCommon(n, descriptors)
	if err != nil {
		return nil, nil, err
	}
	return &Node{
		ID:          n.ID,
		Index:       order,
		Kind:        kind,
		DynamicGVK:  true,
		Subresource: patchSubresource(n),
		Object:      &unstructured.Unstructured{Object: payload},
		Variables:   vars,
		ForEach:     forEach,
		IncludeWhen: includeWhen,
		ReadyWhen:   readyWhen,
	}, nil, nil
}

// isDynamicGVK reports whether a template's apiVersion or kind carries a
// CEL expression, meaning its target GVK can't be known until reconcile
// time. Detection is a scan for the "${" delimiter — apiVersion and kind
// are plain GVK strings, so any expression marker is unambiguous.
func isDynamicGVK(payload map[string]interface{}) bool {
	for _, field := range []string{"apiVersion", "kind"} {
		if s, ok := payload[field].(string); ok && strings.Contains(s, "${") {
			return true
		}
	}
	return false
}

// validateDynamicTemplateStructure checks a dynamic-GVK template or patch has
// the minimum shape: apiVersion + kind present as non-empty strings (their
// content is a CEL expression, resolved at runtime) and a metadata object.
// Unlike validateKubernetesObjectStructure it does not parse apiVersion as
// group/version or enforce the version regex.
func validateDynamicTemplateStructure(obj map[string]interface{}) error {
	if obj == nil {
		return fmt.Errorf("payload is empty")
	}
	for _, field := range []string{"apiVersion", "kind"} {
		v, ok := obj[field]
		if !ok {
			return fmt.Errorf("missing required field %q", field)
		}
		if s, ok := v.(string); !ok || s == "" {
			return fmt.Errorf("field %q must be a non-empty string", field)
		}
	}
	md, ok := obj["metadata"]
	if !ok {
		return fmt.Errorf("missing required field \"metadata\"")
	}
	if _, ok := md.(map[string]interface{}); !ok {
		return fmt.Errorf("field \"metadata\" must be an object")
	}
	return nil
}

// hasMetadataSelector reports whether a projected payload carries a
// metadata.selector object — the marker distinguishing a selector externalRef
// (a read-only collection) from a single-object externalRef.
func hasMetadataSelector(payload map[string]interface{}) bool {
	md, ok := payload["metadata"].(map[string]interface{})
	if !ok {
		return false
	}
	_, ok = md["selector"]
	return ok
}

// projectPayload converts the discriminated-union API node into a single
// unstructured map suitable for CEL extraction.
func projectPayload(n *expv1alpha1.Node) (NodeKind, map[string]interface{}, error) {
	switch {
	case n.Template != nil:
		obj, err := unmarshalRaw(n.Template.Raw)
		if err != nil {
			return 0, nil, fmt.Errorf("template: %w", err)
		}
		return NodeKindTemplate, obj, nil
	case n.Ref != nil:
		obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(n.Ref)
		if err != nil {
			return 0, nil, fmt.Errorf("ref: %w", err)
		}
		return NodeKindRef, obj, nil
	case n.Def != nil:
		obj, err := unmarshalRaw(n.Def.Raw)
		if err != nil {
			return 0, nil, fmt.Errorf("def: %w", err)
		}
		return NodeKindDef, obj, nil
	case n.Patch != nil:
		obj, err := projectPatchPayload(n.Patch)
		if err != nil {
			return 0, nil, fmt.Errorf("patch: %w", err)
		}
		return NodeKindPatch, obj, nil
	default:
		return 0, nil, fmt.Errorf("no payload set")
	}
}

// projectPatchPayload merges a PatchSpec into a single manifest-shaped map:
// the target apiVersion/kind/metadata identity plus the contributed body's
// top-level fields. The result flows through the same GVK-resolution and CEL
// extraction pipeline as a template, so a patch body is type-checked against
// the target schema and its CEL references become dependencies. Body keys may
// not override apiVersion/kind/metadata — those identify the target.
func projectPatchPayload(p *expv1alpha1.PatchSpec) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	if p.Body != nil {
		body, err := unmarshalRaw(p.Body.Raw)
		if err != nil {
			return nil, fmt.Errorf("body: %w", err)
		}
		for k, v := range body {
			out[k] = v
		}
	}
	out["apiVersion"] = p.APIVersion
	out["kind"] = p.Kind
	metadata := map[string]interface{}{"name": p.Metadata.Name}
	if p.Metadata.Namespace != "" {
		metadata["namespace"] = p.Metadata.Namespace
	}
	out["metadata"] = metadata
	return out, nil
}

func unmarshalRaw(raw []byte) (map[string]interface{}, error) {
	if len(raw) == 0 {
		return map[string]interface{}{}, nil
	}
	out := map[string]interface{}{}
	if err := yaml.UnmarshalStrict(raw, &out); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return out, nil
}

// validateKubernetesObjectStructure ensures the payload looks like a K8s
// object: apiVersion + kind set as non-empty strings and apiVersion's
// version segment matches the Kubernetes versioning convention. When
// requireMetadata is true the payload must also carry a metadata object —
// this is true for user-authored Templates but false for Ref
// payloads which are synthesized from typed structs.
func validateKubernetesObjectStructure(obj map[string]interface{}, requireMetadata bool) error {
	if obj == nil {
		return fmt.Errorf("payload is empty")
	}
	for _, field := range []string{"apiVersion", "kind"} {
		v, ok := obj[field]
		if !ok {
			return fmt.Errorf("missing required field %q", field)
		}
		if s, ok := v.(string); !ok || s == "" {
			return fmt.Errorf("field %q must be a non-empty string", field)
		}
	}
	apiVersion, _ := obj["apiVersion"].(string)
	gv, err := k8sschema.ParseGroupVersion(apiVersion)
	if err != nil {
		return fmt.Errorf("apiVersion %q: %w", apiVersion, err)
	}
	if gv.Version == "" {
		return fmt.Errorf("apiVersion %q: missing version", apiVersion)
	}
	if requireMetadata {
		md, ok := obj["metadata"]
		if !ok {
			return fmt.Errorf("missing required field \"metadata\"")
		}
		if _, ok := md.(map[string]interface{}); !ok {
			return fmt.Errorf("field \"metadata\" must be an object")
		}
	}
	return nil
}

// nestedString looks up a dotted path in obj and returns the string value
// at that location, or "" if any segment is missing / wrong type. Used to
// peek at metadata.namespace before kicking off the full resolver pipeline.
func nestedString(obj map[string]interface{}, path ...string) string {
	cur := any(obj)
	for _, seg := range path {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return ""
		}
		cur, ok = m[seg]
		if !ok {
			return ""
		}
	}
	s, _ := cur.(string)
	return s
}

// extractGVKFromUnstructured parses apiVersion/kind into a GVK.
func extractGVKFromUnstructured(obj map[string]interface{}) (k8sschema.GroupVersionKind, error) {
	apiVersion, _ := obj["apiVersion"].(string)
	kind, _ := obj["kind"].(string)
	gv, err := k8sschema.ParseGroupVersion(apiVersion)
	if err != nil {
		return k8sschema.GroupVersionKind{}, fmt.Errorf("parse apiVersion %q: %w", apiVersion, err)
	}
	return gv.WithKind(kind), nil
}

// fieldDescriptorsToVariables wraps every parsed CEL field as a static
// variable; the dependency pass promotes them to Dynamic/Iteration kinds.
func fieldDescriptorsToVariables(descriptors []variable.FieldDescriptor) []*variable.ResourceField {
	if len(descriptors) == 0 {
		return nil
	}
	out := make([]*variable.ResourceField, 0, len(descriptors))
	for _, fd := range descriptors {
		out = append(out, &variable.ResourceField{
			Kind:            variable.ResourceVariableKindStatic,
			FieldDescriptor: fd,
		})
	}
	return out
}

// parseForEachDimensions compiles each {name → expression} entry into a
// ForEachDimension. The expression is parsed (but not yet type-checked).
func parseForEachDimensions(dims []expv1alpha1.ForEachDimension) ([]ForEachDimension, error) {
	if len(dims) == 0 {
		return nil, nil
	}
	out := make([]ForEachDimension, 0, len(dims))
	for i, dim := range dims {
		for name, expr := range dim {
			parsed, err := parser.UnwrapExpressions([]string{expr})
			if err != nil {
				return nil, fmt.Errorf("forEach[%d] %q: %w", i, name, err)
			}
			if len(parsed) != 1 {
				return nil, fmt.Errorf("forEach[%d] %q: expected one expression, got %d", i, name, len(parsed))
			}
			out = append(out, ForEachDimension{Name: name, Expression: parsed[0]})
		}
	}
	return out, nil
}

// parseConditions parses the API node's IncludeWhen and ReadyWhen string
// lists into compiled Expressions. The CEL programs are populated later
// by validateAndCompileNode.
func parseConditions(n *expv1alpha1.Node) (includeWhen, readyWhen []*krocel.Expression, err error) {
	if len(n.IncludeWhen) > 0 {
		includeWhen, err = parser.UnwrapExpressions(n.IncludeWhen)
		if err != nil {
			return nil, nil, fmt.Errorf("includeWhen: %w", err)
		}
	}
	if len(n.ReadyWhen) > 0 {
		readyWhen, err = parser.UnwrapExpressions(n.ReadyWhen)
		if err != nil {
			return nil, nil, fmt.Errorf("readyWhen: %w", err)
		}
	}
	return includeWhen, readyWhen, nil
}
