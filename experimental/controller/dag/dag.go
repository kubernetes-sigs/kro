package dag

import (
	"container/heap"
	"errors"
	"fmt"
	"strings"

	"github.com/kubernetes-sigs/kro/experimental/controller/graph"
)

// ErrCircularDependency indicates that the dependency graph contains a cycle.
// Used by BuildDAG and DetectCyclesEarly.
var ErrCircularDependency = errors.New("circular dependency")

// indexHeap is a min-heap of node indices for Kahn's algorithm.
// Lower index = earlier declaration in spec.nodes = higher priority.
type indexHeap []int

func (h indexHeap) Len() int           { return len(h) }
func (h indexHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h indexHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *indexHeap) Push(x any)        { *h = append(*h, x.(int)) }
func (h *indexHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// DAG builds a dependency graph from a list of Nodes by scanning their
// CEL expressions for variable references. It provides topological ordering
// and dependency-aware planning for the reconcile loop.
type DAG struct {
	// Nodes in declaration order (same as spec.nodes). Per-instance: contains
	// the node bodies with instance-specific concrete values.
	Nodes []graph.Node
	// Topology holds the shared structural information derived from
	// expression dependencies. Immutable after BuildDAG. Multiple instances
	// with the same compilation key share a single topology via AssembleDAG.
	*Topology
}

// Topology holds the structural information derived from expression
// dependencies during compilation. It is a compilation artifact — immutable
// after BuildDAG, shared across all instances with the same compilation key.
//
// Per 004-compilation.md § Structural Compilation Caching: topology is
// determined by expression references, not concrete values. Separating it
// enables O(1) compilation per unique template instead of O(N) per instance.
type Topology struct {
	// Index from node ID to node index
	Index map[string]int
	// TopologicalOrder is the apply order (respects dependencies).
	// Computed via Kahn's algorithm with a min-heap keyed by declaration
	// index. Stable with respect to spec.nodes ordering within each
	// topological level.
	TopologicalOrder []int
	// NodeTypes maps node ID to its declared node type (template, patch,
	// ref, watch, def). Set at compile time from the node's keyword.
	NodeTypes map[string]graph.NodeType
	// Levels groups node indices by topological level. Nodes within
	// the same level are independent and can be processed in parallel.
	// Level 0 has no dependencies, level 1 depends only on level 0, etc.
	Levels [][]int
	// Dependents maps a node ID to the indices of nodes that depend on it.
	// Reverse adjacency list for eager scheduling — when a node completes,
	// check its dependents to see if they can be dispatched.
	Dependents map[string][]int
	// Finalizers maps a target node ID to the IDs of nodes that declare
	// `finalizes` pointing at it. These nodes are created only when the
	// target becomes a prune candidate. The DAG records the relationship
	// but finalization logic lives in the prune phase, not the walk.
	Finalizers map[string][]string

	// Per-node dependency metadata. Indexed by declaration order (same as
	// Nodes). These are derived from expression paths during compilation
	// and are the same across all instances with the same compilation key.
	nodeDeps          []map[string]graph.DepKind      // node index → dependency IDs + kind
	nodeDepPaths      []map[string][]graph.FieldPath  // node index → dep ID → field paths
	nodeSelfPaths     [][]graph.FieldPath             // node index → self field paths
}

// NodeDeps returns the dependency set for a node at the given index.
// Used by tests to verify topology isolation across shared instances.
func (t *Topology) NodeDeps(idx int) map[string]graph.DepKind {
	if idx < 0 || idx >= len(t.nodeDeps) {
		return nil
	}
	return t.nodeDeps[idx]
}

// BuildDAG constructs a dependency graph from a node list.
// exprPaths contains pre-extracted field paths from CEL ASTs (computed during
// compilation in compileGraphSpec). These replace string-scanning with AST-walked
// field paths per 005-reconciliation.md § Hash Mechanics.
// exprAccessModes contains per-expression, per-scope-variable access mode
// classification from pre-rewrite CEL ASTs. Drives DepKind: optional-only
// access → DepLazy, any direct access → DepHard. Nil means all deps are hard.
// Returns an error if the dependency graph contains a cycle (ErrCircularDependency).
// Topological order is computed via Kahn's algorithm with a min-heap keyed by
// declaration index, so independent nodes preserve their spec.nodes ordering.
func BuildDAG(nodes []graph.Node, exprPaths map[string]map[string][]graph.FieldPath, exprAccessModes map[string]map[string]bool) (*DAG, error) {
	topo := &Topology{
		Index:             make(map[string]int, len(nodes)),
		NodeTypes:         make(map[string]graph.NodeType),
		Dependents:        make(map[string][]int),
		Finalizers:        make(map[string][]string),
		nodeDeps:          make([]map[string]graph.DepKind, len(nodes)),
		nodeDepPaths:      make([]map[string][]graph.FieldPath, len(nodes)),
		nodeSelfPaths:     make([][]graph.FieldPath, len(nodes)),
	}

	dag := &DAG{
		Nodes:    make([]graph.Node, len(nodes)),
		Topology: topo,
	}

	for i, node := range nodes {
		var err error
		node.Dependencies, node.DepPaths, node.SelfPaths, err = graph.ExtractReferencedPathsFromNode(node, exprPaths, exprAccessModes)
		if err != nil {
			return nil, err
		}
		if node.ForEach != nil && exprPaths != nil {
			node.ForEach.CollectionSource = resolveCollectionSource(node, exprPaths)
		}
		dag.Nodes[i] = node
		topo.Index[node.ID] = i
		topo.NodeTypes[node.ID] = node.Type()
		// Store per-node dependency metadata in topology for AssembleDAG.
		topo.nodeDeps[i] = node.Dependencies
		topo.nodeDepPaths[i] = node.DepPaths
		topo.nodeSelfPaths[i] = node.SelfPaths
	}

	// Build finalizer map: target node ID → list of finalizer node IDs.
	// Validate that finalizer targets exist in the DAG and manage resources.
	for _, node := range dag.Nodes {
		if node.Finalizes != "" {
			targetIdx, exists := dag.Index[node.Finalizes]
			if !exists {
				return nil, fmt.Errorf("node %q declares finalizes %q, but no node with that ID exists", node.ID, node.Finalizes)
			}
			// Finalization only applies to resource-managing nodes (Template,
			// Patch). Definition, Ref, and Watch nodes never produce managed
			// resources and never become prune candidates — finalizing them
			// is nonsensical.
			targetRef := dag.NodeTypes[dag.Nodes[targetIdx].ID]
		switch targetRef {
		case graph.NodeTypeDef:
			return nil, fmt.Errorf("node %q cannot finalize %q: Definition nodes do not manage resources", node.ID, node.Finalizes)
		case graph.NodeTypeRef:
			return nil, fmt.Errorf("node %q cannot finalize %q: Ref nodes are read-only", node.ID, node.Finalizes)
		case graph.NodeTypeWatch:
			return nil, fmt.Errorf("node %q cannot finalize %q: Watch nodes are read-only", node.ID, node.Finalizes)
			}
			dag.Finalizers[node.Finalizes] = append(dag.Finalizers[node.Finalizes], node.ID)
		}
	}

	// Push downstream dependency paths into upstream SelfPaths.
	// If node B references deploy.status.availableReplicas, the deploy node
	// needs ["status", "availableReplicas"] in its SelfPaths so self-state
	// changes are detected and the updated scope propagates to B. Without
	// this, a bare Template node with no readyWhen/propagateWhen would have empty
	// SelfPaths — status changes would be invisible to downstream consumers.
	for _, node := range dag.Nodes {
		for depID, paths := range node.DepPaths {
			depIdx, exists := dag.Index[depID]
			if !exists {
				continue
			}
			for _, p := range paths {
				graph.AddFieldPath(&dag.Nodes[depIdx].SelfPaths, p)
				graph.AddFieldPath(&topo.nodeSelfPaths[depIdx], p)
			}
		}
	}

	// Build reverse adjacency list: for each node, record which nodes depend on it.
	// Both hard and lazy dependencies create Dependents entries — propagation
	// triggering uses the unified Dependents index.
	for i, node := range dag.Nodes {
		for depID := range node.Dependencies {
			if _, exists := dag.Index[depID]; exists {
				dag.Dependents[depID] = append(dag.Dependents[depID], i)
			}
		}
	}

	// Validate propagateWhen: reject self-references for scalar nodes.
	// propagateWhen on a scalar node is an input gate — it runs before the
	// node evaluates, so the node's own data is not in scope. Self-referencing
	// expressions would deadlock (node can't evaluate to produce data its
	// gate requires).
	//
	// forEach nodes are exempt because their propagateWhen evaluates per-item
	// inside the expansion loop, where the partially-built collection IS in
	// scope. Self-reference is the intended pattern — the gate inspects
	// items already processed in this cycle to decide whether to continue.
	if exprPaths != nil {
		for _, node := range dag.Nodes {
			if node.ForEach != nil {
				continue
			}
			for _, pw := range node.PropagateWhen {
				pos := 0
				for {
					dollars, expr, start, _ := graph.FindExpr(pw, pos)
					if start < 0 {
						break
					}
					pos = start + len(dollars) + len(expr) + 2
					if len(dollars) != 1 {
						continue
					}
					if paths, ok := exprPaths[expr]; ok {
						if _, selfRef := paths[node.ID]; selfRef {
							return nil, fmt.Errorf("node %q: propagateWhen expression %q references itself — "+
								"propagateWhen is an input gate evaluated before the node processes, "+
								"so the node's own data is not in scope. Use a cross-node reference "+
								"(e.g., ${dependency.ready()}) instead", node.ID, pw)
						}
					}
					// Also catch direct .ready() self-references like ${node.ready()}.
					// This does NOT flag .ready() inside comprehensions (e.g.,
					// ${node.dependencies().all(d, d.ready())}) because the
					// check is for the specific pattern "<nodeID>.ready()".
					if strings.Contains(expr, node.ID+".ready()") {
						return nil, fmt.Errorf("node %q: propagateWhen expression %q references its own .ready() — "+
							"propagateWhen is an input gate evaluated before the node processes, "+
							"so the node's own readiness is not available. Use a cross-node reference "+
							"(e.g., ${dependency.ready()}) instead", node.ID, pw)
					}
				}
			}
		}
	}

	// Kahn's algorithm with min-heap: topological sort with cycle detection.
	// The min-heap is keyed by declaration index so that among nodes whose
	// dependencies are all satisfied, the one declared earliest in spec.nodes
	// is emitted first. This makes TopologicalOrder stable with respect to
	// input ordering — independent nodes appear in declaration order.
	// inDegree counts how many in-graph dependencies each node has.
	// inDegree counts how many hard in-graph dependencies each node has.
	// Lazy deps do not contribute to topological ordering.
	n := len(nodes)
	inDegree := make([]int, n)
	for i, node := range dag.Nodes {
		for depID, kind := range node.Dependencies {
			if kind == graph.DepHard {
				if _, exists := dag.Index[depID]; exists {
					inDegree[i]++
				}
			}
		}
	}

	// Seed the heap with nodes that have no in-graph dependencies.
	ready := make(indexHeap, 0, n)
	for i, d := range inDegree {
		if d == 0 {
			ready = append(ready, i)
		}
	}
	heap.Init(&ready)

	order := make([]int, 0, n)
	for ready.Len() > 0 {
		curr := heap.Pop(&ready).(int)
		order = append(order, curr)

		currID := dag.Nodes[curr].ID
		// Decrement in-degree for every node that has a hard dep on curr.
		// Dependents includes both hard and lazy deps; only hard deps
		// contribute to topological ordering (in-degree).
		for _, depIdx := range dag.Dependents[currID] {
			if dag.Nodes[depIdx].Dependencies[currID] == graph.DepHard {
				inDegree[depIdx]--
				if inDegree[depIdx] == 0 {
					heap.Push(&ready, depIdx)
				}
			}
		}
	}

	if len(order) != n {
		// Nodes remaining with non-zero in-degree form the cycle.
		var cycleIDs []string
		for i, d := range inDegree {
			if d > 0 {
				cycleIDs = append(cycleIDs, dag.Nodes[i].ID)
			}
		}
		return nil, fmt.Errorf("graph contains a cycle: nodes %v: %w", cycleIDs, ErrCircularDependency)
	}

	dag.TopologicalOrder = order

	// Compute topological levels. Level[i] = max(Level[hard dep] + 1).
	// Nodes with no hard dependencies are level 0. Lazy deps don't
	// affect level computation (they don't gate dispatch).
	nodeLevel := make([]int, n)
	maxLevel := 0
	for _, idx := range order {
		level := 0
		for depID, kind := range dag.Nodes[idx].Dependencies {
			if kind == graph.DepHard {
				if depIdx, ok := dag.Index[depID]; ok {
					if nodeLevel[depIdx]+1 > level {
						level = nodeLevel[depIdx] + 1
					}
				}
			}
		}
		nodeLevel[idx] = level
		if level > maxLevel {
			maxLevel = level
		}
	}
	dag.Levels = make([][]int, maxLevel+1)
	for idx, level := range nodeLevel {
		dag.Levels[level] = append(dag.Levels[level], idx)
	}

	return dag, nil
}

// resolveCollectionSource determines whether a forEach node's collection
// expression references exactly one scope variable that is NOT referenced
// anywhere else on the node (template body, readyWhen, propagateWhen,
// includeWhen). When this holds, changes to the collection source via
// CollectionChange are safe to handle per-item — the template only
// references items through the iteration variable, never the source directly.
//
// Returns the scope variable ID, or empty string if not optimizable.
func resolveCollectionSource(node graph.Node, exprPaths map[string]map[string][]graph.FieldPath) string {
	// Step 1: Extract scope variables referenced in ForEach.Expr.
	forEachVars := extractScopeVars(node.ForEach.Expr, node.ID, exprPaths)
	if len(forEachVars) != 1 {
		return "" // multiple or zero scope vars — not optimizable
	}
	var candidate string
	for v := range forEachVars {
		candidate = v
	}

	// Step 2: Check if the candidate appears in any other expression on
	// the node — template body, includeWhen, readyWhen, propagateWhen,
	// TemplateExpr. If it does, a collection change affects every item's
	// rendered output, not just the changed item.
	var otherStrs []string
	for _, body := range []map[string]any{node.Template, node.Patch, node.Ref, node.Watch, node.Def} {
		if body != nil {
			graph.CollectStrings(body, &otherStrs)
		}
	}
	if node.TemplateExpr != "" {
		otherStrs = append(otherStrs, node.TemplateExpr)
	}
	otherStrs = append(otherStrs, node.IncludeWhen...)
	otherStrs = append(otherStrs, node.ReadyWhen...)
	otherStrs = append(otherStrs, node.PropagateWhen...)

	for _, s := range otherStrs {
		vars := extractScopeVars(s, node.ID, exprPaths)
		if vars[candidate] {
			return "" // collection source used in template — not safe
		}
	}

	return candidate
}

// extractScopeVars returns the set of scope variable IDs referenced in a
// string that may contain ${...} CEL expressions. Self-references (nodeID)
// are excluded.
func extractScopeVars(s string, nodeID string, exprPaths map[string]map[string][]graph.FieldPath) map[string]bool {
	vars := map[string]bool{}
	pos := 0
	for {
		dollars, expr, start, _ := graph.FindExpr(s, pos)
		if start < 0 {
			break
		}
		pos = start + len(dollars) + len(expr) + 2
		if len(dollars) != 1 {
			continue // $${...} deferred
		}
		if paths, ok := exprPaths[expr]; ok {
			for scopeVar := range paths {
				if scopeVar != nodeID {
					vars[scopeVar] = true
				}
			}
		} else {
			// Fallback: string-based extraction
			id := graph.ExtractFirstIdentifier(expr)
			if id != "" && id != nodeID {
				vars[id] = true
			}
		}
	}
	return vars
}

// AssembleDAG builds a per-instance DAG from an instance's nodes and a shared
// Topology. The topology was computed during compilation and is shared
// across all instances with the same compilation key. This function is
// infallible by construction — all validation (cycles, self-references,
// finalizer targets) was performed during BuildDAG.
//
// Per-node dependency metadata (Dependencies, DepPaths, SelfPaths)
// is deep-copied from the topology to prevent mutation of
// one instance's DAG from corrupting the shared topology. The topology
// itself (Index, TopologicalOrder, Levels, Dependents, Finalizers,
// NodeTypes) is shared by pointer — it is immutable after BuildDAG.
//
// Per 004-compilation.md § Structural Compilation Caching: "Per-instance DAG
// construction takes: shared sort order + shared edges + per-instance node
// specs, and assembles the structure the reconcile loop consumes."
func AssembleDAG(nodes []graph.Node, topo *Topology) *DAG {
	dag := &DAG{
		Nodes:    make([]graph.Node, len(nodes)),
		Topology: topo,
	}
	for i, node := range nodes {
		// Deep-copy per-node dependency metadata from the shared topology.
		// The topology slices must not be mutated through the per-instance
		// DAG — concurrent instances sharing the same topology would corrupt.
		node.Dependencies = copyDepKindMap(topo.nodeDeps[i])
		node.DepPaths = copyDepPaths(topo.nodeDepPaths[i])
		node.SelfPaths = copySelfPaths(topo.nodeSelfPaths[i])
		dag.Nodes[i] = node
	}
	return dag
}

func copyDepKindMap(m map[string]graph.DepKind) map[string]graph.DepKind {
	if m == nil {
		return nil
	}
	c := make(map[string]graph.DepKind, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func copyDepPaths(m map[string][]graph.FieldPath) map[string][]graph.FieldPath {
	if m == nil {
		return nil
	}
	c := make(map[string][]graph.FieldPath, len(m))
	for k, v := range m {
		cp := make([]graph.FieldPath, len(v))
		copy(cp, v)
		c[k] = cp
	}
	return c
}

func copySelfPaths(s []graph.FieldPath) []graph.FieldPath {
	if s == nil {
		return nil
	}
	c := make([]graph.FieldPath, len(s))
	copy(c, s)
	return c
}

// DetectCyclesEarly builds a preliminary dependency graph from expression text
// scanning (no CEL parsing) and rejects cycles before expensive compilation.
// Uses extractFirstIdentifier to extract root scope references from expression
// strings. This is an approximation — it may produce false edges (function
// names look like identifiers) but never misses a real dependency.
//
// Per 004-compilation.md § Algorithm step 1: "Scan expressions for references
// without CEL parsing. Build a DAG. Cycles rejected."
func DetectCyclesEarly(nodes []graph.Node, allIDs []string) error {
	idSet := make(map[string]bool, len(allIDs))
	for _, id := range allIDs {
		idSet[id] = true
	}

	// Build node index and adjacency.
	nodeIndex := make(map[string]int, len(nodes))
	for i, n := range nodes {
		nodeIndex[n.ID] = i
	}

	inDegree := make([]int, len(nodes))
	adj := make([][]int, len(nodes)) // forward adjacency: dep → dependent

	addEdge := func(fromID string, toIdx int) {
		fromIdx, ok := nodeIndex[fromID]
		if !ok || fromIdx == toIdx {
			return // unknown ID or self-reference (harmless for cycle detection)
		}
		adj[fromIdx] = append(adj[fromIdx], toIdx)
		inDegree[toIdx]++
	}

	for i, node := range nodes {
		// Scan all expression-bearing strings in the node's body.
		var strs []string
		if body := node.Body(); body != nil {
			graph.CollectStrings(body, &strs)
		}
		if node.TemplateExpr != "" {
			strs = append(strs, node.TemplateExpr)
		}
		strs = append(strs, node.IncludeWhen...)
		strs = append(strs, node.ReadyWhen...)
		strs = append(strs, node.PropagateWhen...)
		if node.ForEach != nil {
			strs = append(strs, node.ForEach.Expr)
		}

		// Extract expression references.
		seen := make(map[string]bool)
		for _, s := range strs {
			pos := 0
			for {
				dollars, expr, start, _ := graph.FindExpr(s, pos)
				if start < 0 {
					break
				}
				pos = start + len(dollars) + len(expr) + 2
				if len(dollars) != 1 {
					continue // deferred — not a dependency at this level
				}
				rootID := graph.ExtractFirstIdentifier(expr)
				if rootID == "" || !idSet[rootID] || seen[rootID] {
					continue
				}
				seen[rootID] = true
				addEdge(rootID, i)
			}
		}
	}

	// Kahn's algorithm — cycle detection only (no ordering needed).
	queue := make([]int, 0, len(nodes))
	for i, d := range inDegree {
		if d == 0 {
			queue = append(queue, i)
		}
	}
	processed := 0
	for len(queue) > 0 {
		idx := queue[0]
		queue = queue[1:]
		processed++
		for _, dep := range adj[idx] {
			inDegree[dep]--
			if inDegree[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}
	if processed < len(nodes) {
		var cycleIDs []string
		for i, d := range inDegree {
			if d > 0 {
				cycleIDs = append(cycleIDs, nodes[i].ID)
			}
		}
		return fmt.Errorf("graph contains a cycle: nodes %v (detected before compilation): %w", cycleIDs, ErrCircularDependency)
	}
	return nil
}
