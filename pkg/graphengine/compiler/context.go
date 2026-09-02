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

	// localPatchIDs is the subset of localIDs whose node is a Patch. Patch
	// nodes contribute fields to a target they do not own and publish no value
	// into scope, so they cannot be referenced in CEL expressions — including
	// by a nested child frame that captures the ancestor ID. Populated up front
	// (alongside localIDs) so the cross-frame check works even while an
	// ancestor frame is still mid-build.
	localPatchIDs map[string]struct{}

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
	selfWatchExempt     map[string]struct{}

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
		localPatchIDs:       map[string]struct{}{},
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
		localPatchIDs:       map[string]struct{}{},
		nodeSchemaOverrides: ctx.nodeSchemaOverrides,
		literalNodes:        ctx.literalNodes,
		softDepNodes:        ctx.softDepNodes,
		dataPendingTolerant: ctx.dataPendingTolerant,
		selfWatchExempt:     ctx.selfWatchExempt,
		costLimit:           ctx.costLimit,
	}
}

// framePatchKind reports whether id resolves — following lexical
// shadowing (nearest frame wins) — to a Patch node in this frame or any
// enclosing frame. It mirrors frameDepth's walk so an ancestor capture of a
// patch node is rejected the same way a local reference is.
func (ctx *CompilationContext) framePatchKind(id string) bool {
	for c := ctx; c != nil; c = c.parent {
		if _, ok := c.localIDs[id]; ok {
			// Nearest frame wins: once id is declared here, that
			// declaration shadows any same-named ancestor node.
			_, isPatch := c.localPatchIDs[id]
			return isPatch
		}
	}
	return false
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
// buildDefNode builds a Def node from its literal payload. Def nodes have no
// target GVK, but we infer an OpenAPI schema from the payload so the typed CEL
// env can narrow def-sourced expressions; fields whose value is a CEL fragment
// stay dyn (see inferDefSchema). Literal def nodes (e.g. instance schema nodes)
// skip expression parsing.
func (ctx *CompilationContext) buildDefNode(n *expv1alpha1.Node, order int, payload map[string]any) (*Node, *spec.Schema, error) {
	var descriptors []variable.FieldDescriptor
	if _, isLiteral := ctx.literalNodes[n.ID]; !isLiteral {
		var err error
		descriptors, _, err = parser.ParseSchemalessResource(payload)
		if err != nil {
			return nil, nil, fmt.Errorf("parse def payload: %w", err)
		}
	}
	common, err := parseNodeCommon(n, descriptors)
	if err != nil {
		return nil, nil, err
	}
	node := &Node{
		ID:          n.ID,
		Index:       order,
		Kind:        NodeKindDef,
		Object:      &unstructured.Unstructured{Object: payload},
		Variables:   common.Variables,
		ForEach:     common.ForEach,
		IncludeWhen: common.IncludeWhen,
		ReadyWhen:   common.ReadyWhen,
	}
	if override, ok := ctx.nodeSchemaOverrides[n.ID]; ok {
		return node, override, nil
	}
	return node, inferDefSchema(payload), nil
}

func (ctx *CompilationContext) buildNode(p *parser.Parser, n *expv1alpha1.Node, order int) (*Node, *spec.Schema, error) {
	kind, payload, err := projectPayload(n)
	if err != nil {
		return nil, nil, err
	}

	// Def nodes have no target GVK; they are built entirely from the literal
	// payload (see buildDefNode).
	if kind == NodeKindDef {
		return ctx.buildDefNode(n, order, payload)
	}

	// A Template, Ref, or Patch whose apiVersion or kind is a CEL expression
	// has no compile-time GVK: we can't resolve a schema, a REST mapping, or
	// type-check the payload against a target shape. Parse it schemaless,
	// flag it dynamic, and let the executor resolve the concrete GVK
	// per rendered object at apply time. Cross-node references inside the
	// payload are still type-checked later against the typed env; only the
	// node's own field types fall back to dyn. A dynamic Ref stays read-only
	// (single object or selector collection) exactly like a static Ref.
	if (kind == NodeKindTemplate || kind == NodeKindRef || kind == NodeKindPatch) && isDynamicGVK(payload) {
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
	if kind == NodeKindPatch {
		if err := validatePatchPayload(payload); err != nil {
			return nil, nil, err
		}
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
		return nil, nil, fmt.Errorf("rest mapping for %s: %w", gvk, err)
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

	common, err := parseNodeCommon(n, descriptors)
	if err != nil {
		return nil, nil, err
	}

	var subresource string
	if kind == NodeKindPatch {
		subresource, err = derivePatchEndpoint(payload)
		if err != nil {
			return nil, nil, err
		}
	}

	return &Node{
		ID:          n.ID,
		Index:       order,
		Kind:        kind,
		GVR:         mapping.Resource,
		Namespaced:  mapping.Scope.Name() == meta.RESTScopeNameNamespace,
		Subresource: subresource,
		Object:      &unstructured.Unstructured{Object: payload},
		Variables:   common.Variables,
		ForEach:     common.ForEach,
		IncludeWhen: common.IncludeWhen,
		ReadyWhen:   common.ReadyWhen,
		// A Ref whose payload carries metadata.selector is a read-only
		// collection of external objects (list-by-selector), so it publishes
		// a list into scope like a forEach collection.
		Collection: kind == NodeKindRef && hasMetadataSelector(payload),
	}, sch, nil
}

type parsedNodeElements struct {
	Variables   []*variable.ResourceField
	ForEach     []ForEachDimension
	IncludeWhen []*krocel.Expression
	ReadyWhen   []*krocel.Expression
}

func parseNodeCommon(n *expv1alpha1.Node, descriptors []variable.FieldDescriptor) (parsedNodeElements, error) {
	forEach, err := parseForEachDimensions(n.ForEach)
	if err != nil {
		return parsedNodeElements{}, err
	}
	includeWhen, readyWhen, err := parseConditions(n)
	if err != nil {
		return parsedNodeElements{}, err
	}
	return parsedNodeElements{
		Variables:   fieldDescriptorsToVariables(descriptors),
		ForEach:     forEach,
		IncludeWhen: includeWhen,
		ReadyWhen:   readyWhen,
	}, nil
}

// derivePatchEndpoint computes the target subresource for a patch node from
// its unmarshalled manifest payload, per the field-presence rules:
//
//   - identity is metadata.name (required, non-empty) and metadata.namespace
//     (optional).
//   - mainContribution is any top-level key other than apiVersion, kind,
//     metadata, status, OR a metadata key other than name/namespace (so
//     metadata.labels/annotations count as main contributions).
//   - statusContribution is a top-level status key.
//
// A patch must target exactly one endpoint: mixing status with main-resource
// fields is rejected, and identity-only patches (no contributed fields) are
// rejected. Returns "" for the main resource or "status" for the status
// subresource.
func derivePatchEndpoint(payload map[string]any) (string, error) {
	name := nestedString(payload, "metadata", "name")
	if name == "" {
		return "", fmt.Errorf("patch target requires metadata.name")
	}

	mainContribution := false
	statusContribution := false
	for key := range payload {
		switch key {
		case "apiVersion", "kind", "metadata":
			// identity, not a contribution
		case "status":
			statusContribution = true
		default:
			mainContribution = true
		}
	}
	if md, ok := payload["metadata"].(map[string]any); ok {
		for key := range md {
			if key != "name" && key != "namespace" {
				mainContribution = true
				break
			}
		}
	}

	switch {
	case statusContribution && mainContribution:
		return "", fmt.Errorf("a patch node targets a single endpoint: status fields cannot be combined with spec/metadata/other fields in one patch node — split them into separate patch nodes")
	case !statusContribution && !mainContribution:
		return "", fmt.Errorf("patch node contributes no fields")
	case statusContribution:
		return "status", nil
	default:
		return "", nil
	}
}

// validatePatchPayload rejects fields a patch node must never contribute,
// independent of target GVK (so it runs for literal- and dynamic-GVK patches):
//
//   - Identity/lifecycle metadata: metadata.ownerReferences, finalizers,
//     deletionTimestamp, uid. A patch contributes fields without owning the
//     target; these could make the GC delete or terminate a target the patch
//     does not own, breaking the CRD's "never deletes its target" guarantee.
func validatePatchPayload(payload map[string]any) error {
	md, ok := payload["metadata"].(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range []string{"ownerReferences", "finalizers", "deletionTimestamp", "uid"} {
		if _, ok := md[key]; ok {
			return fmt.Errorf("patch node payload must not set metadata.%s: a patch contributes fields under a dedicated field manager without owning the target and must never delete it, so identity/lifecycle metadata (ownerReferences, finalizers, deletionTimestamp, uid) is rejected", key)
		}
	}
	return nil
}

// buildDynamicNode compiles a Template or Patch whose apiVersion or kind is
// a CEL expression. There is no compile-time GVK, so we skip schema
// resolution and REST mapping, parse the payload schemaless, and mark the
// node dynamic. metadata is still required (payloads are authored) and
// apiVersion/kind must be non-empty strings, but the version segment isn't
// validated — it isn't a literal version yet. The node publishes no schema
// (nil), so downstream references see it as dyn until the executor pins the
// GVK.
func (ctx *CompilationContext) buildDynamicNode(
	n *expv1alpha1.Node,
	order int,
	payload map[string]any,
	kind NodeKind,
) (*Node, *spec.Schema, error) {
	if err := validateDynamicTemplateStructure(payload); err != nil {
		return nil, nil, err
	}
	descriptors, _, err := parser.ParseSchemalessResource(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("parse dynamic %s payload: %w", kind, err)
	}
	common, err := parseNodeCommon(n, descriptors)
	if err != nil {
		return nil, nil, err
	}
	var subresource string
	if kind == NodeKindPatch {
		if err := validatePatchPayload(payload); err != nil {
			return nil, nil, err
		}
		subresource, err = derivePatchEndpoint(payload)
		if err != nil {
			return nil, nil, err
		}
	}
	return &Node{
		ID:          n.ID,
		Index:       order,
		Kind:        kind,
		DynamicGVK:  true,
		Subresource: subresource,
		Object:      &unstructured.Unstructured{Object: payload},
		Variables:   common.Variables,
		ForEach:     common.ForEach,
		IncludeWhen: common.IncludeWhen,
		ReadyWhen:   common.ReadyWhen,
		// A dynamic Ref carrying metadata.selector is a read-only collection
		// (list-by-selector), same as the static Ref path. Template/Patch are
		// never collections here, so the guard is a no-op for them.
		Collection: kind == NodeKindRef && hasMetadataSelector(payload),
	}, nil, nil
}

// isDynamicGVK reports whether a template's apiVersion or kind carries a
// CEL expression, meaning its target GVK can't be known until reconcile
// time. Detection is a scan for the "${" delimiter — apiVersion and kind
// are plain GVK strings, so any expression marker is unambiguous.
func isDynamicGVK(payload map[string]any) bool {
	for _, field := range []string{"apiVersion", "kind"} {
		if s, ok := payload[field].(string); ok && strings.Contains(s, "${") {
			return true
		}
	}
	return false
}

// validateDynamicTemplateStructure checks a dynamic-GVK template, ref, or
// patch has the minimum shape: apiVersion + kind present as non-empty strings
// (their content is a CEL expression, resolved at runtime) and a metadata
// object. A dynamic ref carries metadata (name or selector), so this is the
// right shape check for it too. Unlike validateKubernetesObjectStructure it
// does not parse apiVersion as group/version or enforce the version regex.
func validateDynamicTemplateStructure(obj map[string]any) error {
	if obj == nil {
		return fmt.Errorf("payload is empty")
	}
	if err := requireNonEmptyStringFields(obj, "apiVersion", "kind"); err != nil {
		return err
	}
	return requireMetadataObject(obj)
}

// requireNonEmptyStringFields checks each named field is present in obj and
// holds a non-empty string.
func requireNonEmptyStringFields(obj map[string]any, fields ...string) error {
	for _, field := range fields {
		v, ok := obj[field]
		if !ok {
			return fmt.Errorf("missing required field %q", field)
		}
		if s, ok := v.(string); !ok || s == "" {
			return fmt.Errorf("field %q must be a non-empty string", field)
		}
	}
	return nil
}

// requireMetadataObject checks obj carries a metadata object.
func requireMetadataObject(obj map[string]any) error {
	md, ok := obj["metadata"]
	if !ok {
		return fmt.Errorf("missing required field %q", "metadata")
	}
	if _, ok := md.(map[string]any); !ok {
		return fmt.Errorf("field %q must be an object", "metadata")
	}
	return nil
}

// hasMetadataSelector reports whether a projected payload carries a
// metadata.selector object — the marker distinguishing a selector externalRef
// (a read-only collection) from a single-object externalRef.
func hasMetadataSelector(payload map[string]any) bool {
	md, ok := payload["metadata"].(map[string]any)
	if !ok {
		return false
	}
	// A runtime.RawExtension selector always projects a "selector" key, even
	// when unset (its zero value renders as a nil value). Treat only a nil
	// value as absent, mirroring ExternalRefMetadata.HasSelector on the API
	// type: an explicitly-set selector — including an empty one that matches
	// everything — marks a collection ref.
	sel, ok := md["selector"]
	return ok && sel != nil
}

// hasMetadataName reports whether a projected ref payload carries a non-empty
// metadata.name — the marker of a single-object externalRef.
func hasMetadataName(payload map[string]any) bool {
	md, ok := payload["metadata"].(map[string]any)
	if !ok {
		return false
	}
	name, _ := md["name"].(string)
	return name != ""
}

// validateRefMetadata enforces the exactly-one contract on a ref node's
// metadata: a ref must set metadata.name (single object) OR metadata.selector
// (collection), but not both and not neither. This mirrors the CRD-level
// XValidation on ExternalRefMetadata ("exactly one of name or selector must be
// provided") so an inline Graph compiled directly — bypassing API-server
// admission — cannot silently drop the name and list by selector, or issue a
// broken empty-name read.
func validateRefMetadata(payload map[string]any) error {
	hasName := hasMetadataName(payload)
	hasSelector := hasMetadataSelector(payload)
	if hasName == hasSelector {
		return fmt.Errorf("exactly one of name or selector must be provided")
	}
	return nil
}

// projectPayload converts the discriminated-union API node into a single
// unstructured map suitable for CEL extraction.
func projectPayload(n *expv1alpha1.Node) (NodeKind, map[string]any, error) {
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
		// Enforce the exactly-one-of name/selector contract here, on the shared
		// projection path, so it applies to inline Graphs compiled directly
		// (bypassing the CRD XValidation on ExternalRefMetadata) as well as to
		// both the static and dynamic ref compile paths.
		if err := validateRefMetadata(obj); err != nil {
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
		obj, err := unmarshalRaw(n.Patch.Raw)
		if err != nil {
			return 0, nil, fmt.Errorf("patch: %w", err)
		}
		return NodeKindPatch, obj, nil
	default:
		return 0, nil, fmt.Errorf("no payload set")
	}
}

func unmarshalRaw(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	out := map[string]any{}
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
func validateKubernetesObjectStructure(obj map[string]any, requireMetadata bool) error {
	if obj == nil {
		return fmt.Errorf("payload is empty")
	}
	if err := requireNonEmptyStringFields(obj, "apiVersion", "kind"); err != nil {
		return err
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
		if err := requireMetadataObject(obj); err != nil {
			return err
		}
	}
	return nil
}

// nestedString looks up a dotted path in obj and returns the string value
// at that location, or "" if any segment is missing / wrong type. Used to
// peek at metadata.namespace before kicking off the full resolver pipeline.
func nestedString(obj map[string]any, path ...string) string {
	cur := any(obj)
	for _, seg := range path {
		m, ok := cur.(map[string]any)
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
func extractGVKFromUnstructured(obj map[string]any) (k8sschema.GroupVersionKind, error) {
	apiVersion, _ := obj["apiVersion"].(string)
	kind, _ := obj["kind"].(string)
	if apiVersion == "" {
		return k8sschema.GroupVersionKind{}, fmt.Errorf("missing or invalid apiVersion")
	}
	if kind == "" {
		return k8sschema.GroupVersionKind{}, fmt.Errorf("missing or invalid kind")
	}
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
