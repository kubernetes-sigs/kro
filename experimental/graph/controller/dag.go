package graphcontroller

import "fmt"

// DAG builds a dependency graph from a list of Resources by scanning their
// CEL expressions for variable references. It provides topological ordering
// and dependency-aware planning for the reconcile loop.
type DAG struct {
	// Nodes in declaration order (same as spec.resources)
	Nodes []DAGNode
	// Index from resource ID to node index
	Index map[string]int
	// TopologicalOrder is the apply order (respects dependencies).
	// Computed via Kahn's algorithm — declaration order is not significant.
	TopologicalOrder []int
	// ReverseOrder is the delete order (reverse of TopologicalOrder).
	ReverseOrder []int
	// Contributions are resource IDs that were detected as contributions
	// from the template shape (only apiVersion/kind/metadata/status keys).
	Contributions map[string]bool
}

// DAGNode represents a resource in the dependency graph.
type DAGNode struct {
	Resource Resource
	// Dependencies are IDs of resources this node references in its expressions
	Dependencies map[string]bool
}

// BuildDAG constructs a dependency graph from a resource list.
// Dependencies are extracted by scanning CEL expressions for variable references.
// Returns an error if the dependency graph contains a cycle (ErrCycleDetected).
// Declaration order is not significant — topological order is computed from
// the dependency graph via Kahn's algorithm.
func BuildDAG(resources []Resource) (*DAG, error) {
	dag := &DAG{
		Nodes:         make([]DAGNode, len(resources)),
		Index:         make(map[string]int, len(resources)),
		Contributions: make(map[string]bool),
	}

	for i, res := range resources {
		refs := extractReferencedIDs(res)
		dag.Nodes[i] = DAGNode{
			Resource:     res,
			Dependencies: refs,
		}
		dag.Index[res.ID] = i

		// Detect contributions from template shape: a template whose keys
		// are a subset of {apiVersion, kind, metadata, status} is a
		// contribution — it underspecifies the resource, writing only
		// metadata and/or status fields on an object someone else owns.
		if res.Template != nil && isContributionTemplate(res.Template) {
			dag.Contributions[res.ID] = true
		}
	}

	// Kahn's algorithm: topological sort with cycle detection.
	// inDegree counts how many in-graph dependencies each node has.
	n := len(resources)
	inDegree := make([]int, n)
	for i, node := range dag.Nodes {
		for depID := range node.Dependencies {
			if _, exists := dag.Index[depID]; exists {
				inDegree[i]++
			}
		}
	}

	// Seed the queue with nodes that have no in-graph dependencies.
	var queue []int
	for i, d := range inDegree {
		if d == 0 {
			queue = append(queue, i)
		}
	}

	var order []int
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		order = append(order, curr)

		currID := dag.Nodes[curr].Resource.ID
		// Decrement in-degree for every node that depends on curr.
		for i, node := range dag.Nodes {
			if node.Dependencies[currID] {
				inDegree[i]--
				if inDegree[i] == 0 {
					queue = append(queue, i)
				}
			}
		}
	}

	if len(order) != n {
		// Nodes remaining with non-zero in-degree form the cycle.
		var cycleIDs []string
		for i, d := range inDegree {
			if d > 0 {
				cycleIDs = append(cycleIDs, dag.Nodes[i].Resource.ID)
			}
		}
		return nil, fmt.Errorf("resources %v form a dependency cycle: %w", cycleIDs, ErrCycleDetected)
	}

	dag.TopologicalOrder = order
	dag.ReverseOrder = make([]int, n)
	for i, idx := range order {
		dag.ReverseOrder[n-1-i] = idx
	}

	return dag, nil
}

// isContributionTemplate checks if a template's field structure indicates
// a contribution. A template that contains only apiVersion, kind, metadata,
// and/or status fields (no spec or other resource-specific fields) is a
// contribution — it underspecifies the resource, writing only metadata
// and/or status on an object someone else manages.
//
// This implements one of the two detection criteria from the design:
// "specifying only status and metadata." The other criterion — "omitting
// required fields" via OpenAPI schema comparison — is not yet implemented.
// A template that writes a partial spec (some spec fields but not all
// required ones) will NOT be detected as a contribution by this check.
// When OpenAPI detection is added, it will catch those cases.
func isContributionTemplate(tmpl map[string]any) bool {
	if len(tmpl) == 0 {
		return false
	}
	for key := range tmpl {
		switch key {
		case "apiVersion", "kind", "metadata", "status":
			continue
		default:
			return false
		}
	}
	return true
}

// NodeState tracks the reconcile-time state of a single node.
type NodeState int

const (
	NodePending     NodeState = iota // Not yet processed
	NodeReady                        // Applied and readyWhen satisfied
	NodeNotReady                     // Applied but readyWhen not satisfied
	NodeExcluded                     // Excluded by includeWhen or contagious exclusion
	NodeDataPending                  // CEL expression couldn't resolve (retryable)
	NodeError                        // Fatal error
	NodeConflict                     // SSA 409 — field ownership taken by another actor
)

// PlanState tracks the state of all nodes during a reconcile cycle.
type PlanState struct {
	States map[string]NodeState
}

// NewPlanState creates a fresh plan state with all nodes pending.
func NewPlanState(dag *DAG) *PlanState {
	ps := &PlanState{
		States: make(map[string]NodeState, len(dag.Nodes)),
	}
	for _, node := range dag.Nodes {
		ps.States[node.Resource.ID] = NodePending
	}
	return ps
}

// CanProcess returns true if the node's dependencies are all in a state
// that allows this node to proceed. A node can proceed if all its
// dependencies are Ready or not referenced at all.
//
// Returns (canProcess, blockingNodeID).
func (ps *PlanState) CanProcess(node *DAGNode) (bool, string) {
	for depID := range node.Dependencies {
		state, exists := ps.States[depID]
		if !exists {
			continue // not a scope variable (could be a CEL builtin)
		}
		switch state {
		case NodeReady:
			continue // good
		case NodePending:
			// Dependency hasn't been processed yet — shouldn't happen
			// in topological order, but be safe
			return false, depID
		case NodeNotReady:
			return false, depID
		case NodeExcluded:
			return false, depID
		case NodeDataPending:
			return false, depID
		case NodeError:
			return false, depID
		case NodeConflict:
			return false, depID
		}
	}
	return true, ""
}

// SetState updates a node's state and propagates contagious exclusion.
// When a node is excluded, all nodes that depend on it are also excluded.
func (ps *PlanState) SetState(dag *DAG, id string, state NodeState) {
	ps.States[id] = state

	// Propagate contagious exclusion
	if state == NodeExcluded || state == NodeDataPending || state == NodeError || state == NodeConflict {
		ps.propagateExclusion(dag, id)
	}
}

// propagateExclusion marks all downstream dependents of a node as excluded.
func (ps *PlanState) propagateExclusion(dag *DAG, excludedID string) {
	for _, node := range dag.Nodes {
		if ps.States[node.Resource.ID] != NodePending {
			continue // already processed
		}
		if node.Dependencies[excludedID] {
			ps.States[node.Resource.ID] = NodeExcluded
			// Recurse: this node's dependents are also excluded
			ps.propagateExclusion(dag, node.Resource.ID)
		}
	}
}

// Summary returns aggregate state for status reporting.
func (ps *PlanState) Summary() (hasDataPending, hasNotReady, hasExcluded, hasError, hasConflict bool, readyCount int) {
	for _, state := range ps.States {
		switch state {
		case NodeReady:
			readyCount++
		case NodeNotReady:
			hasNotReady = true
		case NodeDataPending:
			hasDataPending = true
		case NodeExcluded:
			hasExcluded = true
		case NodeError:
			hasError = true
		case NodeConflict:
			hasConflict = true
		}
	}
	return
}
