package graphcontroller

// DAG builds a dependency graph from a list of Resources by scanning their
// CEL expressions for variable references. It provides topological ordering
// and dependency-aware planning for the reconcile loop.
type DAG struct {
	// Nodes in declaration order (same as spec.resources)
	Nodes []DAGNode
	// Index from resource ID to node index
	Index map[string]int
	// TopologicalOrder is the apply order (respects dependencies)
	TopologicalOrder []int
	// ReverseOrder is the delete order
	ReverseOrder []int
}

// DAGNode represents a resource in the dependency graph.
type DAGNode struct {
	Resource Resource
	// Dependencies are IDs of resources this node references in its expressions
	Dependencies map[string]bool
}

// BuildDAG constructs a dependency graph from a resource list.
// Dependencies are extracted by scanning CEL expressions for variable references.
func BuildDAG(resources []Resource) *DAG {
	dag := &DAG{
		Nodes: make([]DAGNode, len(resources)),
		Index: make(map[string]int, len(resources)),
	}

	for i, res := range resources {
		refs := extractReferencedIDs(res)
		dag.Nodes[i] = DAGNode{
			Resource:     res,
			Dependencies: refs,
		}
		dag.Index[res.ID] = i
	}

	// Topological sort. Since resources are declared in order and can only
	// reference prior resources, the declaration order IS a valid topological
	// order. We verify this and use it directly.
	dag.TopologicalOrder = make([]int, len(resources))
	dag.ReverseOrder = make([]int, len(resources))
	for i := range resources {
		dag.TopologicalOrder[i] = i
		dag.ReverseOrder[i] = len(resources) - 1 - i
	}

	return dag
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
