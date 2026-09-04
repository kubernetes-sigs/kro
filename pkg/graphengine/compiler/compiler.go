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
	"net/http"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/discovery"
	memcache "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/kube-openapi/pkg/validation/spec"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/ast"
	"github.com/kubernetes-sigs/kro/pkg/dag"
	"github.com/kubernetes-sigs/kro/pkg/features"
	"github.com/kubernetes-sigs/kro/pkg/graph/parser"
	schemaresolver "github.com/kubernetes-sigs/kro/pkg/graph/schema/resolver"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

// Compiler turns a v1alpha1.Graph into a compiled Program. It owns the
// long-lived schema resolver and REST mapper; per Compile it builds a fresh
// CompilationContext (see context.go) that carries those plus a per-compile
// field cache and the lexical frame chain. Node-level work — GVK resolution,
// schema lookup, the static-vs-dynamic-GVK decision — lives on the context;
// the Compiler orchestrates the whole-graph passes (dependency DAG, type
// checking, schema-dependency emission).
//
// The resolverCache field is the live cached resolver under the combined
// resolver — held separately so the schema watcher can drive InvalidateSchema
// directly into it on CRD content changes. Nil when the Compiler is
// constructed via NewCompilerWithDependencies (i.e. tests); production callers
// go through NewCompiler.
//
// discoveryCache/deferredMapper are the memory-cached discovery client and the
// DeferredDiscoveryRESTMapper wrapping it (only set by NewCompiler). The
// deferred mapper is held so InvalidateSchema can Reset() the REST mapping /
// discovery caches when a CRD's identity changes — see the note there. Nil
// under NewCompilerWithDependencies.
type Compiler struct {
	schemaResolver resolver.SchemaResolver
	restMapper     meta.RESTMapper
	resolverCache  *schemaresolver.CachedSchemaResolver
	deferredMapper *restmapper.DeferredDiscoveryRESTMapper
	costLimit      uint64
}

// NewCompiler constructs a Compiler from a rest.Config. The supplied
// httpClient is used by the schema resolver and the discovery REST mapper.
func NewCompiler(cfg *rest.Config, httpClient *http.Client) (*Compiler, error) {
	sr, cached, err := schemaresolver.NewCombinedResolverWithCache(cfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("create schema resolver: %w", err)
	}
	// Use a DeferredDiscoveryRESTMapper backed by a memory-cached discovery
	// client instead of controller-runtime's apiutil.NewDynamicRESTMapper.
	// The latter only reloads a group on a NoMatch error, so recreating a CRD
	// with the same GroupKind+version but a new scope (Namespaced<->Cluster) or
	// plural leaves the stale mapping in place until restart. The deferred
	// mapper exposes Reset(), which InvalidateSchema calls to drop the cached
	// discovery data and force fresh rediscovery.
	dc, err := discovery.NewDiscoveryClientForConfigAndClient(cfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("create discovery client: %w", err)
	}
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memcache.NewMemCacheClient(dc))
	c := NewCompilerWithDependencies(sr, rm)
	c.resolverCache = cached
	c.deferredMapper = rm
	return c, nil
}

// NewCompilerWithDependencies builds a Compiler directly from an already-
// constructed schema resolver and REST mapper. Useful for tests and
// callers that wire these themselves (e.g. against a fake discovery
// client). InvalidateSchema is a no-op on Compilers built this way
// (there is no cached resolver to evict from).
func NewCompilerWithDependencies(sr resolver.SchemaResolver, rm meta.RESTMapper) *Compiler {
	return &Compiler{schemaResolver: sr, restMapper: rm}
}

// WithCostLimit sets the CEL evaluation cost limit for compiled expressions.
// Setting costLimit to 0 disables cost limiting (default).
func (c *Compiler) WithCostLimit(limit uint64) *Compiler {
	c.costLimit = limit
	return c
}

// rootContext builds a fresh root CompilationContext for a single Compile.
// It carries the compiler's shared schema resolver and REST mapper plus a
// per-compile field cache; parent is nil (the root lexical frame).
func (c *Compiler) rootContext() *CompilationContext {
	ctx := newRootContext(c.schemaResolver, c.restMapper)
	ctx.costLimit = c.costLimit
	return ctx
}

// InvalidateSchema drops cached schema entries for the supplied
// GroupKind from the compiler's resolver cache, so the next compile
// re-fetches fresh data. The schema watcher calls this when a CRD's
// content changes. No-op when the compiler was built without a
// cached resolver (tests).
//
// It also resets the REST mapping / discovery caches. The mapper is a
// DeferredDiscoveryRESTMapper whose delegate caches every GVR->scope/plural
// mapping; that cache only self-heals on a NoMatch, so recreating a CRD with
// the same GroupKind+version but a new scope (Namespaced<->Cluster) or plural
// would otherwise keep routing to the stale endpoint until restart. The mapper
// has no per-GroupKind eviction, so we do a full Reset() (invalidates the
// discovery cache and drops the delegate mapper); it re-discovers lazily on the
// next mapping request. Schema invalidations are rare (CRD content changes), so
// the cost of a full re-discovery here is acceptable.
func (c *Compiler) InvalidateSchema(gk k8sschema.GroupKind) {
	if c.resolverCache == nil {
		return
	}
	c.resolverCache.InvalidateGroupKind(gk)
	if c.deferredMapper != nil {
		c.deferredMapper.Reset()
	}
}

// CompileOption customizes a single CompileWithOptions call.
type CompileOption func(*compileOptions)

type compileOptions struct {
	nodeSchemaOverrides map[string]*spec.Schema
	literalNodes        map[string]struct{}
	softDepNodes        map[string]struct{}
	dataPendingTolerant map[string]struct{}
	selfWatchExempt     map[string]struct{}
}

// WithLiteralNode marks a node (such as a Def node) as pure literal data,
// skipping expression parsing inside its payload.
func WithLiteralNode(nodeID string) CompileOption {
	return func(o *compileOptions) {
		if o.literalNodes == nil {
			o.literalNodes = make(map[string]struct{})
		}
		o.literalNodes[nodeID] = struct{}{}
	}
}

// WithNodeSchemaOverride declares the OpenAPI schema a node publishes into
// scope. For Def nodes this replaces the value-shape inference the compiler
// otherwise applies (a fresh instance carrying no fields must still
// type-check expressions that reference them). The schema is used verbatim;
// the caller owns any conversion (e.g. RGD SimpleSchema → OpenAPI).
func WithNodeSchemaOverride(nodeID string, s *spec.Schema) CompileOption {
	return func(o *compileOptions) {
		if o.nodeSchemaOverrides == nil {
			o.nodeSchemaOverrides = make(map[string]*spec.Schema)
		}
		o.nodeSchemaOverrides[nodeID] = s
	}
}

// WithSoftDependencies marks a node so that every local resource reference it
// carries is classified as a soft (non-gating) dependency: the node gets no
// DAG edge to those resources and does not gate on them, exactly as if each
// reference were optional-chained. The referenced nodes are still seeded with
// an empty object in scope, so an unresolved reference data-pends per field
// rather than failing the whole graph. Used by the RGD adapter for the
// synthesized author-status writeback node, which must observe resources as
// they become available without ordering the reconcile around them.
func WithSoftDependencies(nodeID string) CompileOption {
	return func(o *compileOptions) {
		if o.softDepNodes == nil {
			o.softDepNodes = make(map[string]struct{})
		}
		o.softDepNodes[nodeID] = struct{}{}
	}
}

// WithDataPendingTolerant marks a node so that a field whose expression is
// data-pending is omitted from the rendered object rather than failing the
// whole node. The node still applies its remaining resolved fields. Used with
// WithSoftDependencies for the author-status writeback node so status fields
// appear progressively, mirroring ProjectInstanceStatus's per-field skip.
func WithDataPendingTolerant(nodeID string) CompileOption {
	return func(o *compileOptions) {
		if o.dataPendingTolerant == nil {
			o.dataPendingTolerant = make(map[string]struct{})
		}
		o.dataPendingTolerant[nodeID] = struct{}{}
	}
}

// WithSelfWatchExempt marks a node so the executor does NOT register a drift
// watch on its target. Used for the synthesized author-status writeback patch
// node, which targets the reconciled instance's own status subresource: a
// status write bumps resourceVersion (not generation), and the drift-watch
// enqueue path is not generation-guarded, so watching the instance the node
// writes would re-enqueue the instance on its own status write — a
// self-perpetuating reconcile loop. The instance's own parent informer already
// drives reconciliation, so the self-watch is redundant as well as harmful.
func WithSelfWatchExempt(nodeID string) CompileOption {
	return func(o *compileOptions) {
		if o.selfWatchExempt == nil {
			o.selfWatchExempt = make(map[string]struct{})
		}
		o.selfWatchExempt[nodeID] = struct{}{}
	}
}

// Compile validates the Graph, parses every node's CEL expressions against
// the target schemas, builds the dependency DAG, and returns the compiled
// Program. Nested subgraphs are compiled recursively, each in its own lexical
// frame. The input Graph is not mutated.
func (c *Compiler) Compile(g *expv1alpha1.Graph) (*Program, error) {
	return c.CompileWithOptions(g)
}

// CompileWithOptions is Compile with caller-supplied options.
func (c *Compiler) CompileWithOptions(g *expv1alpha1.Graph, opts ...CompileOption) (*Program, error) {
	if err := validateGraph(g); err != nil {
		return nil, fmt.Errorf("invalid graph: %w", err)
	}
	graph := g.DeepCopy()
	var co compileOptions
	for _, opt := range opts {
		opt(&co)
	}
	ctx := c.rootContext()
	ctx.nodeSchemaOverrides = co.nodeSchemaOverrides
	ctx.literalNodes = co.literalNodes
	ctx.softDepNodes = co.softDepNodes
	ctx.dataPendingTolerant = co.dataPendingTolerant
	ctx.selfWatchExempt = co.selfWatchExempt
	prog, _, err := ctx.compileFrame(graph.Spec.Nodes, true)
	if err != nil {
		return nil, err
	}
	return prog, nil
}

// compileFrame compiles one lexical frame — the top-level Graph (isRoot) or a
// nested subgraph — into a Program. It returns the Program plus the set of
// captured ancestor-frame node IDs referenced from within this frame. The
// caller (the owning subgraph node) attaches those as dependencies so the
// executor runs the subgraph only after the captured parent nodes resolve.
func (ctx *CompilationContext) compileFrame(apiNodes []expv1alpha1.Node, isRoot bool) (*Program, []string, error) {
	if !isRoot {
		if err := validateFrameNodes(apiNodes); err != nil {
			return nil, nil, fmt.Errorf("invalid subgraph: %w", err)
		}
	}

	// Declare every local ID up front so a child frame compiled mid-build
	// (when we recurse into a subgraph node below) can resolve forward
	// references to this frame's later nodes. Record which of those are Patch
	// nodes too, so an ancestor capture of a patch node is rejected even while
	// this frame is still mid-build.
	for i := range apiNodes {
		ctx.localIDs[apiNodes[i].ID] = struct{}{}
		if apiNodes[i].Patch != nil {
			ctx.localPatchIDs[apiNodes[i].ID] = struct{}{}
		}
	}

	p := parser.New(ctx.fieldCache)
	nodes := make(map[string]*Node, len(apiNodes))
	nodeSchemas := make(map[string]*spec.Schema, len(apiNodes))
	var captured []string

	for i := range apiNodes {
		apiNode := &apiNodes[i]
		if apiNode.Graph != nil {
			node, bubble, err := ctx.buildSubgraphNode(apiNode, i)
			if err != nil {
				return nil, nil, fmt.Errorf("build node %q: %w", apiNode.ID, err)
			}
			nodes[node.ID] = node
			captured = append(captured, bubble...)
			continue
		}
		built, sch, err := ctx.buildNode(p, apiNode, i)
		if err != nil {
			return nil, nil, fmt.Errorf("build node %q: %w", apiNode.ID, err)
		}
		if _, ok := ctx.dataPendingTolerant[built.ID]; ok {
			built.TolerateDataPending = true
		}
		if _, ok := ctx.selfWatchExempt[built.ID]; ok {
			built.SelfWatchExempt = true
		}
		nodes[built.ID] = built
		if sch != nil {
			nodeSchemas[built.ID] = sch
		}
	}

	// The resolvable-root check is root-only and needs the built DAG, so it
	// runs after buildDependencyGraph below.

	// The inspector environment knows every local ID, every visible ancestor
	// ID (captures), iterator names, and `each`. Ancestor refs are valid
	// identifiers here; the dependency pass classifies them as captures.
	ancestors := ctx.ancestorIDs()
	identifiers := make([]string, 0, len(apiNodes)+len(ancestors)+1)
	for i := range apiNodes {
		identifiers = append(identifiers, apiNodes[i].ID)
	}
	identifiers = append(identifiers, ancestors...)
	identifiers = append(identifiers, EachVarName)
	identifiers = append(identifiers, allIteratorNames(nodes)...)
	identifiers = dedupe(identifiers)

	inspectorEnv, err := krocel.DefaultEnvironment(
		krocel.WithResourceIDs(identifiers),
		krocel.WithRuntimeLibrary(false),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("build inspector environment: %w", err)
	}
	inspector := ast.NewInspectorWithEnv(inspectorEnv, identifiers)

	dependencyGraph, frameCaptured, err := ctx.buildDependencyGraph(nodes, inspector)
	if err != nil {
		return nil, nil, fmt.Errorf("build dependency graph: %w", err)
	}
	captured = append(captured, frameCaptured...)
	// A subgraph may legitimately be driven entirely by captures from its
	// parent, so it has no local root; only the top-level Graph must have a
	// resolvable root of its own. Cycles (at any depth) are caught by the
	// topological sort below.
	if isRoot {
		if err := requireResolvableRoot(dependencyGraph); err != nil {
			return nil, nil, err
		}
	}
	topo, err := dependencyGraph.TopologicalSort()
	if err != nil {
		return nil, nil, fmt.Errorf("topological sort: %w", err)
	}

	// Wrap collection node schemas as lists so other nodes see them as arrays.
	celSchemas := make(map[string]*spec.Schema, len(nodeSchemas))
	for id, sch := range nodeSchemas {
		// Patch nodes contribute fields to a target they do not own; they
		// publish no value into scope, so they are not typed identifiers.
		if nodes[id].Kind == NodeKindPatch {
			continue
		}
		if nodes[id].IsCollection() {
			celSchemas[id] = ctx.fieldCache.WrapAsList(sch)
		} else {
			celSchemas[id] = sch
		}
	}

	// Def nodes contribute inferred schemas to celSchemas (set in buildNode),
	// so the typed env knows `${naming.prefix}` down to its field type.
	//
	// Three kinds of identifier are declared dyn rather than typed: dynamic-
	// GVK templates and subgraph nodes (local, no published schema), plus all
	// captured ancestor IDs (the cross-frame seam). Within-frame typed
	// references stay fully checked; the rest type-check permissively.
	dynIDs := make([]string, 0, len(ancestors)+len(nodes))
	dynIDs = append(dynIDs, ancestors...)
	for id, n := range nodes {
		if n.Kind == NodeKindPatch {
			continue
		}
		if _, ok := celSchemas[id]; !ok {
			dynIDs = append(dynIDs, id)
		}
	}
	typedEnv, typeProvider, err := krocel.TypedEnvironmentWithIDsAndProvider(
		celSchemas,
		dynIDs,
		krocel.WithRuntimeLibrary(false),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("build typed CEL environment: %w", err)
	}

	bc := newBuildContext(typedEnv, typeProvider, ctx.fieldCache, ctx.costLimit)
	for id, node := range nodes {
		if node.Kind == NodeKindGraph {
			continue // already compiled in its own frame
		}
		var payloadSchema *spec.Schema
		if node.Kind == NodeKindTemplate || node.Kind == NodeKindPatch {
			// A patch body is typed against the target schema exactly as a
			// template is typed against its own manifest schema.
			payloadSchema = nodeSchemas[id]
		}
		// elementSchema is the unwrapped per-node schema (element type for
		// collections), used to type `each` when compiling collection readyWhen.
		elementSchema := nodeSchemas[id]
		if err := validateAndCompileNode(bc, node, payloadSchema, elementSchema); err != nil {
			return nil, nil, fmt.Errorf("compile node %q: %w", id, err)
		}
	}

	prog := &Program{
		DAG:              dependencyGraph,
		Nodes:            nodes,
		TopologicalOrder: topo,
		NodeSchemas:      celSchemas,
	}
	emitSchemaDependencies(prog)
	captured = dedupe(captured)
	return prog, captured, nil
}

// buildSubgraphNode compiles a nested Graph node. The child compiles in a
// fresh frame linked to this one; the captured ancestor IDs it reports are
// partitioned — those declared at THIS frame become the subgraph node's
// dependencies (so the executor seeds the child scope after they resolve),
// the rest bubble further up to this frame's own caller.
func (ctx *CompilationContext) buildSubgraphNode(n *expv1alpha1.Node, order int) (*Node, []string, error) {
	subNodes, err := unmarshalGraphSpec(n.Graph.Raw)
	if err != nil {
		return nil, nil, fmt.Errorf("parse subgraph: %w", err)
	}
	subProg, childCaptured, err := ctx.child().compileFrame(subNodes, false)
	if err != nil {
		return nil, nil, err
	}
	node := &Node{ID: n.ID, Index: order, Kind: NodeKindGraph, SubProgram: subProg}
	var bubble []string
	for _, id := range childCaptured {
		if ctx.frameDepth(id) == 0 {
			addDependency(node, id)
		} else {
			bubble = append(bubble, id)
		}
	}
	return node, bubble, nil
}

// unmarshalGraphSpec parses a subgraph node's RawExtension payload (a
// GraphSpec with a nodes list) into its node slice.
func unmarshalGraphSpec(raw []byte) ([]expv1alpha1.Node, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty subgraph payload")
	}
	var sub expv1alpha1.GraphSpec
	if err := yaml.UnmarshalStrict(raw, &sub); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return sub.Nodes, nil
}

// emitSchemaDependencies walks the compiled nodes and populates
// Program.RequiredGroupKinds (deduplicated) plus Program.HasDynamicGVK
// (true if any node has a CEL expression at the apiVersion or kind
// path). The schema watcher reads these to build its reverse index of
// "which Graphs care about which CRDs."
//
// Def nodes contribute nothing — they don't reference cluster schemas.
// Template/Ref/Patch nodes contribute their target GroupKind. A subgraph
// node folds in its child Program's already-aggregated dependencies, so the
// root Program ends up with the full set across every nesting level — the
// SchemaWatcher tracks all of them.
//
// A dynamic-GVK node (apiVersion or kind is a CEL expression) has no
// literal GroupKind to contribute; it flips HasDynamicGVK instead, which
// makes the SchemaWatcher subscribe the Graph to all CRD changes.
func emitSchemaDependencies(p *Program) {
	seen := make(map[k8sschema.GroupKind]struct{})
	add := func(gk k8sschema.GroupKind) {
		if gk.Kind == "" {
			return
		}
		if _, dup := seen[gk]; dup {
			return
		}
		seen[gk] = struct{}{}
		p.RequiredGroupKinds = append(p.RequiredGroupKinds, gk)
	}
	for _, n := range p.Nodes {
		switch {
		case n.Kind == NodeKindGraph:
			if n.SubProgram == nil {
				continue
			}
			if n.SubProgram.HasDynamicGVK {
				p.HasDynamicGVK = true
			}
			for _, gk := range n.SubProgram.RequiredGroupKinds {
				add(gk)
			}
		case n.Kind == NodeKindDef:
			// no cluster schema
		case n.DynamicGVK:
			p.HasDynamicGVK = true
		case n.Object != nil:
			gv, err := k8sschema.ParseGroupVersion(n.Object.GetAPIVersion())
			if err != nil {
				continue
			}
			add(k8sschema.GroupKind{Group: gv.Group, Kind: n.Object.GetKind()})
		}
	}
}

// isIdentityFieldPath reports whether path identifies a resource's
// identity field. metadata.name is identity for every resource;
// metadata.namespace is identity only for namespaced resources.
func isIdentityFieldPath(path string, namespaced bool) bool {
	switch path {
	case "metadata.name":
		return true
	case "metadata.namespace":
		return namespaced
	}
	return false
}

// allIteratorNames returns the union of iterator variable names declared by
// every node's forEach.
func allIteratorNames(nodes map[string]*Node) []string {
	var names []string
	for _, n := range nodes {
		for _, iter := range n.ForEach {
			names = append(names, iter.Name)
		}
	}
	return names
}

// dedupe removes duplicate strings in place, preserving order.
func dedupe(xs []string) []string {
	if len(xs) == 0 {
		return xs
	}
	seen := make(map[string]struct{}, len(xs))
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	return out
}

// buildDependencyGraph walks every compiled node in this frame, inspects each
// CEL expression for references (local node IDs, iterator variables, and
// captured ancestor IDs), and produces a DAG over the local nodes. It returns
// the deduplicated set of captured ancestor IDs so the caller can attach them
// to the owning subgraph node. Cycles raise an error during topological sort.
func (ctx *CompilationContext) buildDependencyGraph(nodes map[string]*Node, inspector *ast.Inspector) (*dag.DirectedAcyclicGraph[string], []string, error) {
	g := dag.NewDirectedAcyclicGraph[string]()
	// Iterate nodes in a deterministic (sorted-by-ID) order. `nodes` is a map,
	// so ranging it directly registers vertices and dependency edges in an
	// arbitrary order per compile. AddDependencies rejects (and reports) the
	// FIRST cycle-closing edge it sees, so with more than one independent cycle
	// present the reported cycle — and thus the compiled Graph's condition
	// message — varied from reconcile to reconcile, churning status on an
	// unchanged-but-invalid Graph (reviewer finding 3909713187). Sorting the
	// registration order (together with the sorted cycle traversal in dag) makes
	// the reported cycle a stable function of the Graph alone.
	ordered := make([]*Node, 0, len(nodes))
	for _, n := range nodes {
		ordered = append(ordered, n)
	}
	slices.SortFunc(ordered, func(a, b *Node) int { return strings.Compare(a.ID, b.ID) })
	for _, n := range ordered {
		if err := g.AddVertex(n.ID, n.Index); err != nil {
			return nil, nil, fmt.Errorf("add vertex %q: %w", n.ID, err)
		}
	}

	var captured []string
	for _, n := range ordered {
		capt, err := ctx.analyzeNodeRefs(n, inspector)
		if err != nil {
			return nil, nil, fmt.Errorf("node %q: %w", n.ID, err)
		}
		captured = append(captured, capt...)
		if _, soft := ctx.softDepNodes[n.ID]; soft {
			// Reclassify every resource dependency as soft: the node gets no DAG
			// edge and imposes no ordering, so it observes resources as they
			// publish instead of gating the reconcile. Captured ancestor refs are
			// left untouched (this node has none in practice).
			for i := range n.Dependencies {
				n.Dependencies[i].Soft = true
			}
		}
		if err := g.AddDependencies(n.ID, n.HardDepIDs()); err != nil {
			return nil, nil, fmt.Errorf("node %q: register deps: %w", n.ID, err)
		}
	}
	captured = dedupe(captured)
	return g, captured, nil
}

// analyzeNodeRefs inspects every expression a node carries — variables,
// forEach axes, includeWhen, readyWhen — adding internal dependency edges to
// n and returning the captured ancestor IDs referenced across all of them.
func (ctx *CompilationContext) analyzeNodeRefs(n *Node, inspector *ast.Inspector) ([]string, error) {
	var captured []string
	for _, analyze := range []func(*Node, *ast.Inspector) ([]string, error){
		ctx.analyzeVariables,
		ctx.analyzeForEach,
		ctx.analyzeIncludeWhen,
		ctx.analyzeReadyWhen,
	} {
		capt, err := analyze(n, inspector)
		if err != nil {
			return nil, err
		}
		captured = append(captured, capt...)
	}
	return captured, nil
}

// analyzeVariables classifies each variable expression (collecting deps and
// captures), promotes the variable's kind, and enforces that every forEach
// iterator appears in an identity field so rendered instances stay unique.
func (ctx *CompilationContext) analyzeVariables(n *Node, inspector *ast.Inspector) ([]string, error) {
	iteratorNames := nodeIteratorNames(n)
	identityIterators := make(map[string]struct{}, len(iteratorNames))
	var captured []string
	for _, v := range n.Variables {
		analysis, err := ctx.extractDependencies(inspector, v.Expression, iteratorNames)
		if err != nil {
			return nil, fmt.Errorf("variable at %q: %w", v.Path, err)
		}
		captured = append(captured, analysis.captured...)
		if len(analysis.iteratorRefs) > 0 {
			v.Kind = variable.ResourceVariableKindIteration
		} else if (len(analysis.nodeDeps) > 0 || len(analysis.captured) > 0) && v.Kind == variable.ResourceVariableKindStatic {
			v.Kind = variable.ResourceVariableKindDynamic
		}
		for _, d := range analysis.nodeDeps {
			addDependency(n, d)
		}
		if isIdentityFieldPath(v.Path, n.Namespaced) {
			if analysis.inspection != nil && analysis.inspection.UsesOmit() {
				return nil, fmt.Errorf("variable at %q: omit() cannot be used in resource identity fields", v.Path)
			}
			for _, it := range analysis.iteratorRefs {
				identityIterators[it] = struct{}{}
			}
		}
	}
	// Every forEach iterator must appear in an identity field so each rendered
	// instance has a unique GVK+name(+namespace); otherwise SSA apply rejects
	// later instances (template) or the patch lands on one target N times
	// (patch). kro catches it at compile time.
	if len(iteratorNames) > 0 && (n.Kind == NodeKindTemplate || n.Kind == NodeKindPatch) {
		var missing []string
		for _, it := range iteratorNames {
			if _, ok := identityIterators[it]; !ok {
				missing = append(missing, it)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf(
				"every forEach iterator must appear in metadata.name (or metadata.namespace for namespaced resources) to produce unique identities, missing: %v",
				missing,
			)
		}
	}
	return captured, nil
}

// analyzeForEach collects forEach-axis dependencies. Iterator dimensions may
// reference other nodes (local or captured) but not each other.
func (ctx *CompilationContext) analyzeForEach(n *Node, inspector *ast.Inspector) ([]string, error) {
	iteratorNames := nodeIteratorNames(n)
	var captured []string
	for _, dim := range n.ForEach {
		analysis, err := ctx.extractDependencies(inspector, dim.Expression, iteratorNames)
		if err != nil {
			return nil, fmt.Errorf("forEach %q: %w", dim.Name, err)
		}
		if analysis.inspection != nil && analysis.inspection.UsesOmit() {
			return nil, fmt.Errorf("forEach %q: omit() can only be used in resource template expressions", dim.Name)
		}
		if len(analysis.iteratorRefs) > 0 {
			return nil, fmt.Errorf("forEach %q cannot reference other iterators %v", dim.Name, analysis.iteratorRefs)
		}
		captured = append(captured, analysis.captured...)
		for _, d := range analysis.nodeDeps {
			addDependency(n, d)
		}
	}
	return captured, nil
}

// analyzeIncludeWhen collects includeWhen dependencies. A node cannot depend
// on itself here: it must publish (become included) before its own includeWhen
// can be evaluated, so a self-reference is unsatisfiable and is rejected at
// compile time.
func (ctx *CompilationContext) analyzeIncludeWhen(n *Node, inspector *ast.Inspector) ([]string, error) {
	var captured []string
	for i, expr := range n.IncludeWhen {
		analysis, err := ctx.extractDependencies(inspector, expr, nil)
		if err != nil {
			return nil, fmt.Errorf("includeWhen[%d]: %w", i, err)
		}
		if analysis.inspection != nil && analysis.inspection.UsesOmit() {
			return nil, fmt.Errorf("includeWhen[%d]: omit() can only be used in resource template expressions", i)
		}
		captured = append(captured, analysis.captured...)
		for _, d := range analysis.nodeDeps {
			if d == n.ID {
				return nil, fmt.Errorf("node %q: includeWhen references its own id %q, which can never be satisfied", n.ID, n.ID)
			}
			addDependency(n, d)
		}
	}
	return captured, nil
}

// analyzeReadyWhen verifies each readyWhen expression references only the
// node itself — no cross-node deps (local or captured), which would create
// implicit ordering ambiguity.
func (ctx *CompilationContext) analyzeReadyWhen(n *Node, inspector *ast.Inspector) ([]string, error) {
	for i, expr := range n.ReadyWhen {
		analysis, err := ctx.extractDependencies(inspector, expr, nil)
		if err != nil {
			return nil, fmt.Errorf("readyWhen[%d]: %w", i, err)
		}
		if analysis.inspection != nil && analysis.inspection.UsesOmit() {
			return nil, fmt.Errorf("readyWhen[%d]: omit() can only be used in resource template expressions", i)
		}
		if len(analysis.captured) > 0 {
			return nil, fmt.Errorf("readyWhen[%d] (%q) may only reference the node itself, found capture %q", i, expr.UserExpression(), analysis.captured[0])
		}
		for _, d := range analysis.nodeDeps {
			if d != n.ID {
				return nil, fmt.Errorf("readyWhen[%d] (%q) may only reference the node itself, found %q", i, expr.UserExpression(), d)
			}
		}
	}
	return nil, nil
}

type dependencyAnalysis struct {
	inspection   *ast.ExpressionInspection
	nodeDeps     []string
	iteratorRefs []string
	captured     []string
}

// extractDependencies inspects a single expression and classifies every
// referenced identifier against the lexical frame chain:
//
//   - a local node ID (this frame)        -> nodeDeps (an internal DAG edge)
//   - a captured ancestor node ID         -> captured (bubbles to the subgraph)
//   - an iterator variable / each         -> iteratorRefs (frame-neutral)
//   - anything else                       -> an "unknown identifier" error
//
// All resource references within expressions are classified as hard dependencies.
// Soft dependencies are declared explicitly via WithSoftDependencies.
//
// The no-mix rule: an expression's node references must all resolve to a
// single frame. Mixing this graph's nodes with an enclosing graph's nodes in
// one expression is rejected — iterators and `each` are exempt, being
// node-local binding variables rather than a graph scope.
func (ctx *CompilationContext) extractDependencies(
	inspector *ast.Inspector,
	expr *krocel.Expression,
	iteratorNames []string,
) (dependencyAnalysis, error) {
	result, err := inspector.Inspect(expr.Original)
	if err != nil {
		return dependencyAnalysis{}, fmt.Errorf("inspect: %w", err)
	}

	if !features.FeatureGate.Enabled(features.CELOmitFunction) && result.UsesOmit() {
		return dependencyAnalysis{}, fmt.Errorf("omit() requires the CELOmitFunction feature gate to be enabled")
	}

	var nodeDeps []string
	var iteratorRefs []string
	var captured []string

	frames := make(map[int]struct{})
	classify := func(id string) error {
		if id == EachVarName {
			return nil
		}
		// A patch node publishes no value into scope and cannot be referenced
		// in CEL. Resolve across the ancestor frame chain (mirroring
		// frameDepth) so a child frame that captures an ancestor patch node is
		// rejected too — not only references to a patch node in this frame.
		if ctx.framePatchKind(id) {
			return fmt.Errorf("patch node %q does not publish a value into scope and cannot be referenced in CEL expressions", id)
		}
		if !slices.Contains(expr.References, id) {
			expr.References = append(expr.References, id)
		}
		if slices.Contains(iteratorNames, id) {
			if !slices.Contains(iteratorRefs, id) {
				iteratorRefs = append(iteratorRefs, id)
			}
			return nil
		}
		switch d := ctx.frameDepth(id); {
		case d == 0:
			if !slices.Contains(nodeDeps, id) {
				nodeDeps = append(nodeDeps, id)
			}
			frames[0] = struct{}{}
		case d > 0:
			if !slices.Contains(captured, id) {
				captured = append(captured, id)
			}
			frames[d] = struct{}{}
		default:
			return fmt.Errorf("references unknown identifier %q", id)
		}
		return nil
	}

	for _, dep := range result.ResourceDependencies {
		if err := classify(dep.ID); err != nil {
			return dependencyAnalysis{}, err
		}
	}
	for _, unknown := range result.UnknownResources {
		if err := classify(unknown.ID); err != nil {
			return dependencyAnalysis{}, err
		}
	}
	if len(result.UnknownFunctions) > 0 {
		return dependencyAnalysis{}, fmt.Errorf("uses unknown functions: %v", result.UnknownFunctions)
	}
	if len(frames) > 1 {
		return dependencyAnalysis{}, fmt.Errorf(
			"expression %q mixes node references from different graph scopes (references must belong to a single scope)",
			expr.UserExpression(),
		)
	}
	return dependencyAnalysis{
		inspection:   &result,
		nodeDeps:     nodeDeps,
		iteratorRefs: iteratorRefs,
		captured:     captured,
	}, nil
}

func nodeIteratorNames(n *Node) []string {
	if len(n.ForEach) == 0 {
		return nil
	}
	out := make([]string, 0, len(n.ForEach))
	for _, dim := range n.ForEach {
		out = append(out, dim.Name)
	}
	return out
}

// addDependency records an edge from n to dep. If the edge already exists,
// it is a no-op. Order is preserved. Edges are created as hard dependencies
// (Soft: false); soft reclassification is performed explicitly in
// buildDependencyGraph for nodes configured with WithSoftDependencies.
func addDependency(n *Node, dep string) {
	for i := range n.Dependencies {
		if n.Dependencies[i].ID == dep {
			return
		}
	}
	n.Dependencies = append(n.Dependencies, Dependency{ID: dep, Soft: false})
}
