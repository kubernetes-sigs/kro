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

	"golang.org/x/exp/maps"
	"k8s.io/apimachinery/pkg/api/meta"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/rest"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	krocel "github.com/kubernetes-sigs/kro/pkg/graphengine/cel"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/cel/ast"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler/dag"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler/parser"
	schemaresolver "github.com/kubernetes-sigs/kro/pkg/graphengine/compiler/schema/resolver"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler/variable"
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
type Compiler struct {
	schemaResolver resolver.SchemaResolver
	restMapper     meta.RESTMapper
	resolverCache  *schemaresolver.CachedSchemaResolver
}

// NewCompiler constructs a Compiler from a rest.Config. The supplied
// httpClient is used by the schema resolver and the discovery REST mapper.
func NewCompiler(cfg *rest.Config, httpClient *http.Client) (*Compiler, error) {
	sr, cached, err := schemaresolver.NewCombinedResolver(cfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("create schema resolver: %w", err)
	}
	rm, err := apiutil.NewDynamicRESTMapper(cfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("create REST mapper: %w", err)
	}
	c := NewCompilerWithDependencies(sr, rm)
	c.resolverCache = cached
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

// rootContext builds a fresh root CompilationContext for a single Compile.
// It carries the compiler's shared schema resolver and REST mapper plus a
// per-compile field cache; parent is nil (the root lexical frame).
func (c *Compiler) rootContext() *CompilationContext {
	return newRootContext(c.schemaResolver, c.restMapper)
}

// InvalidateSchema drops cached schema entries for the supplied
// GroupKind from the compiler's resolver cache, so the next compile
// re-fetches fresh data. The schema watcher calls this when a CRD's
// content changes. No-op when the compiler was built without a
// cached resolver (tests).
//
// Note: the REST mapper has its own internal cache. controller-
// runtime's dynamic REST mapper handles CRD changes via its standard
// refresh logic; we don't need a separate invalidation hook for it.
func (c *Compiler) InvalidateSchema(gk k8sschema.GroupKind) {
	if c.resolverCache == nil {
		return
	}
	c.resolverCache.InvalidateGroupKind(gk)
}

// Compile validates the Graph, parses every node's CEL expressions against
// the target schemas, builds the dependency DAG, and returns the compiled
// Program. Nested subgraphs are compiled recursively, each in its own lexical
// frame. The input Graph is not mutated.
func (c *Compiler) Compile(g *expv1alpha1.Graph) (*Program, error) {
	if err := validateGraph(g); err != nil {
		return nil, fmt.Errorf("invalid graph: %w", err)
	}
	graph := g.DeepCopy()
	prog, _, err := c.rootContext().compileFrame(graph.Spec.Nodes, true)
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
	// references to this frame's later nodes.
	for i := range apiNodes {
		ctx.localIDs[apiNodes[i].ID] = struct{}{}
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
		nodes[built.ID] = built
		if sch != nil {
			nodeSchemas[built.ID] = sch
		}
	}

	// A seed node is only required at the root: a subgraph may legitimately
	// be driven entirely by captures from its parent.
	if isRoot {
		if err := requireInputNode(nodes); err != nil {
			return nil, nil, err
		}
	}

	// The inspector environment knows every local ID, every visible ancestor
	// ID (captures), iterator names, and `each`. Ancestor refs are valid
	// identifiers here; the dependency pass classifies them as captures.
	ancestors := ctx.ancestorIDs()
	identifiers := maps.Keys(nodes)
	identifiers = append(identifiers, ancestors...)
	identifiers = append(identifiers, EachVarName)
	identifiers = append(identifiers, allIteratorNames(nodes)...)
	dedupe(&identifiers)

	inspectorEnv, err := krocel.DefaultEnvironment(krocel.WithResourceIDs(identifiers))
	if err != nil {
		return nil, nil, fmt.Errorf("build inspector environment: %w", err)
	}
	inspector := ast.NewInspectorWithEnv(inspectorEnv, identifiers)

	dependencyGraph, frameCaptured, err := ctx.buildDependencyGraph(nodes, inspector)
	if err != nil {
		return nil, nil, fmt.Errorf("build dependency graph: %w", err)
	}
	captured = append(captured, frameCaptured...)
	topo, err := dependencyGraph.TopologicalSort()
	if err != nil {
		return nil, nil, fmt.Errorf("topological sort: %w", err)
	}

	// Wrap collection node schemas as lists so other nodes see them as arrays.
	celSchemas := make(map[string]*spec.Schema, len(nodeSchemas))
	for id, sch := range nodeSchemas {
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
	dynIDs := append([]string(nil), ancestors...)
	for id := range nodes {
		if _, ok := celSchemas[id]; !ok {
			dynIDs = append(dynIDs, id)
		}
	}
	typedEnv, typeProvider, err := krocel.TypedEnvironmentWithIDsAndProvider(celSchemas, dynIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("build typed CEL environment: %w", err)
	}

	bc := newBuildContext(typedEnv, typeProvider, ctx.fieldCache)
	for id, node := range nodes {
		if node.Kind == NodeKindGraph {
			continue // already compiled in its own frame
		}
		var payloadSchema *spec.Schema
		if node.Kind == NodeKindTemplate {
			payloadSchema = nodeSchemas[id]
		}
		if err := validateAndCompileNode(bc, node, payloadSchema); err != nil {
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
	dedupe(&captured)
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
// Template/Ref/Watch nodes contribute their target GroupKind. A subgraph
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
func dedupe(xs *[]string) {
	seen := make(map[string]struct{}, len(*xs))
	out := (*xs)[:0]
	for _, x := range *xs {
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	*xs = out
}

// buildDependencyGraph walks every compiled node in this frame, inspects each
// CEL expression for references (local node IDs, iterator variables, and
// captured ancestor IDs), and produces a DAG over the local nodes. It returns
// the deduplicated set of captured ancestor IDs so the caller can attach them
// to the owning subgraph node. Cycles raise an error during topological sort.
func (ctx *CompilationContext) buildDependencyGraph(nodes map[string]*Node, inspector *ast.Inspector) (*dag.DirectedAcyclicGraph[string], []string, error) {
	g := dag.NewDirectedAcyclicGraph[string]()
	for _, n := range nodes {
		if err := g.AddVertex(n.ID, n.Index); err != nil {
			return nil, nil, fmt.Errorf("add vertex %q: %w", n.ID, err)
		}
	}

	var captured []string
	for _, n := range nodes {
		capt, err := ctx.analyzeNodeRefs(n, inspector)
		if err != nil {
			return nil, nil, fmt.Errorf("node %q: %w", n.ID, err)
		}
		captured = append(captured, capt...)
		if err := g.AddDependencies(n.ID, n.Dependencies); err != nil {
			return nil, nil, fmt.Errorf("node %q: register deps: %w", n.ID, err)
		}
	}
	dedupe(&captured)
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
		deps, iterRefs, capt, err := ctx.extractDependencies(inspector, v.Expression, iteratorNames)
		if err != nil {
			return nil, fmt.Errorf("variable at %q: %w", v.Path, err)
		}
		captured = append(captured, capt...)
		if len(iterRefs) > 0 {
			v.Kind = variable.ResourceVariableKindIteration
		} else if (len(deps) > 0 || len(capt) > 0) && v.Kind == variable.ResourceVariableKindStatic {
			v.Kind = variable.ResourceVariableKindDynamic
		}
		for _, d := range deps {
			addDependency(n, d)
		}
		if isIdentityFieldPath(v.Path, n.Namespaced) {
			for _, it := range iterRefs {
				identityIterators[it] = struct{}{}
			}
		}
	}
	// Every forEach iterator must appear in an identity field so each rendered
	// instance has a unique GVK+name(+namespace); otherwise SSA apply rejects
	// later instances. kro catches it at compile time.
	if len(iteratorNames) > 0 && n.Kind == NodeKindTemplate {
		var missing []string
		for _, it := range iteratorNames {
			if _, ok := identityIterators[it]; !ok {
				missing = append(missing, it)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("every forEach iterator must appear in metadata.name (or metadata.namespace for namespaced resources) to produce unique identities; missing: %v", missing)
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
		deps, iterRefs, capt, err := ctx.extractDependencies(inspector, dim.Expression, iteratorNames)
		if err != nil {
			return nil, fmt.Errorf("forEach %q: %w", dim.Name, err)
		}
		if len(iterRefs) > 0 {
			return nil, fmt.Errorf("forEach %q cannot reference other iterators %v", dim.Name, iterRefs)
		}
		captured = append(captured, capt...)
		for _, d := range deps {
			addDependency(n, d)
		}
	}
	return captured, nil
}

// analyzeIncludeWhen collects includeWhen dependencies, dropping a node's
// self-reference so it doesn't create a self-edge in the DAG.
func (ctx *CompilationContext) analyzeIncludeWhen(n *Node, inspector *ast.Inspector) ([]string, error) {
	var captured []string
	for i, expr := range n.IncludeWhen {
		deps, _, capt, err := ctx.extractDependencies(inspector, expr, nil)
		if err != nil {
			return nil, fmt.Errorf("includeWhen[%d]: %w", i, err)
		}
		captured = append(captured, capt...)
		for _, d := range deps {
			if d == n.ID {
				continue
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
		deps, _, capt, err := ctx.extractDependencies(inspector, expr, nil)
		if err != nil {
			return nil, fmt.Errorf("readyWhen[%d]: %w", i, err)
		}
		if len(capt) > 0 {
			return nil, fmt.Errorf("readyWhen[%d] (%q) may only reference the node itself, found capture %q", i, expr.UserExpression(), capt[0])
		}
		for _, d := range deps {
			if d != n.ID {
				return nil, fmt.Errorf("readyWhen[%d] (%q) may only reference the node itself, found %q", i, expr.UserExpression(), d)
			}
		}
	}
	return nil, nil
}

// extractDependencies inspects a single expression and classifies every
// referenced identifier against the lexical frame chain:
//
//   - a local node ID (this frame)        -> nodeDeps (an internal DAG edge)
//   - a captured ancestor node ID         -> captured (bubbles to the subgraph)
//   - an iterator variable / each         -> iteratorRefs (frame-neutral)
//   - anything else                       -> an "unknown identifier" error
//
// The no-mix rule: an expression's node references must all resolve to a
// single frame. Mixing this graph's nodes with an enclosing graph's nodes in
// one expression is rejected — iterators and `each` are exempt, being
// node-local binding variables rather than a graph scope.
func (ctx *CompilationContext) extractDependencies(
	inspector *ast.Inspector,
	expr *krocel.Expression,
	iteratorNames []string,
) (nodeDeps []string, iteratorRefs []string, captured []string, err error) {
	result, err := inspector.Inspect(expr.Original)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("inspect: %w", err)
	}

	frames := make(map[int]struct{})
	classify := func(id string) error {
		if id == EachVarName {
			return nil
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
			return nil, nil, nil, err
		}
	}
	for _, unknown := range result.UnknownResources {
		if err := classify(unknown.ID); err != nil {
			return nil, nil, nil, err
		}
	}
	if len(result.UnknownFunctions) > 0 {
		return nil, nil, nil, fmt.Errorf("uses unknown functions: %v", result.UnknownFunctions)
	}
	if len(frames) > 1 {
		return nil, nil, nil, fmt.Errorf("expression %q mixes node references from different graph scopes; an expression may reference one scope (this graph or an enclosing graph), not both", expr.UserExpression())
	}
	return nodeDeps, iteratorRefs, captured, nil
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

func addDependency(n *Node, dep string) {
	if !slices.Contains(n.Dependencies, dep) {
		n.Dependencies = append(n.Dependencies, dep)
	}
}
