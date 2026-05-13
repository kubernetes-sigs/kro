// propagate.go implements the DAG propagation algorithm for a single reconcile cycle.
//
// The propagation evaluates nodes sequentially in topological order. Each node is
// evaluated after all of its hard dependencies have been resolved. The propagation
// is the single writer to shared state (scope, plan, applied keys).
//
// Per 005-reconciliation.md § Reconcile: "The coordinator walks the DAG in
// topological order, dispatching each node when its dependencies are resolved."
package graphcontroller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/cel-go/common/types"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
	graphpkg "github.com/ellistarn/kro/experimental/controller/graph"
)

// nodeResult carries a node's evaluation output back to the propagation loop.
type nodeResult struct {
	keys  []Applied
	state NodeState
	err   error

	// scope is the published value for this node (full K8s object, collection, or definition map).
	scope any

	// resolvedGVK carries the GVK resolved by a dynamic-GVK node's template
	// evaluation. Used to detect staleness per 004-compilation.md § Deferred Types.
	resolvedGVK *schema.GroupVersionKind
}

// propagateResult is the output of a complete DAG propagation, consumed by the reconciler.
type propagateResult struct {
	keys           []Applied            // flattened applied keys from all nodes
	nodeKeys       map[string][]Applied // per-node applied keys (for carry-forward on next reconcile)
	plan           *PlanState    // per-node states
	needsRecompile bool          // dynamic GVK resolved or changed
	nodeErrors     []string      // "nodeID: reason" for status reporting
	summary        PlanSummary
	// NextRequeue is the minimum requeue duration computed by time comparison
	// solving across all blocked propagateWhen gates. Zero means no hint.
	NextRequeue time.Duration
}

// propagate executes a sequential DAG propagation in topological order.
//
// Each node is evaluated after all hard dependencies have completed. Lazy
// dependencies are available as optionals in scope but do not gate evaluation.
// The propagation is sequential — no goroutines, no channels.
func (r *GraphReconciler) propagate(ctx context.Context, rs *reconcileScope, state *instanceState, eval *evaluator, dag *dagpkg.DAG, plan *PlanState) *propagateResult {
	result := &propagateResult{
		plan: plan,
	}

	// Per-node applied keys — flattened into result.keys after the propagation.
	nodeKeys := make(map[string][]Applied, len(dag.Nodes))

	for _, nodeIdx := range dag.TopologicalOrder {
		nrOut := r.propagateNode(ctx, rs, state, eval, dag, plan, nodeKeys, nodeIdx)
		if nrOut != nil {
			result.nodeErrors = append(result.nodeErrors, nrOut.errMsgs...)
			if nrOut.needsRecompile {
				result.needsRecompile = true
			}
			if nrOut.requeueHint > 0 && (result.NextRequeue == 0 || nrOut.requeueHint < result.NextRequeue) {
				result.NextRequeue = nrOut.requeueHint
			}
		}
	}

	// --- Post-propagation ---

	// Flatten per-node keys into the applied key set.
	result.nodeKeys = nodeKeys
	for _, keys := range nodeKeys {
		result.keys = append(result.keys, keys...)
	}

	// Derive aggregate state from the DAG plan.
	result.summary = plan.Summary()

	return result
}

// propagateNode processes a single node in the DAG propagation. It encapsulates
// the per-node sequence: skip finalizers → dependency gating → propagateWhen →
// includeWhen → evaluate → integrate result.
//
// Returns nil when the node was handled without producing integration output
// (skipped, excluded, blocked, pending), or a nodeIntegrationResult when
// evaluation occurred.
func (r *GraphReconciler) propagateNode(
	ctx context.Context,
	rs *reconcileScope,
	state *instanceState,
	eval *evaluator,
	dag *dagpkg.DAG,
	plan *PlanState,
	nodeKeys map[string][]Applied,
	nodeIdx int,
) *nodeIntegrationResult {
	logger := log.FromContext(ctx)
	node := &dag.Nodes[nodeIdx]

	// Finalizer nodes are dormant during normal operation — they only
	// materialize during prune/teardown. Skip them in the forward propagation.
	if node.Finalizes != "" {
		plan.SetState(node.ID, NodeReady)
		return nil
	}

	// --- Dependency gating ---
	// Check hard dependencies. Lazy deps don't gate dispatch or cause
	// exclusion — the expression has a branch that handles absent data.
	gate := checkDependencyGate(node, plan)

	if gate == depExcluded {
		// Contagious exclusion: any hard dependency Excluded → Excluded.
		// Per 005-reconciliation.md § Propagation step 1.
		logger.V(1).Info("node excluded — dependency excluded", "node", node.ID)
		plan.SetState(node.ID, NodeExcluded)
		markExcluded(eval, node.ID, state)
		removeMetricIfExcluded(rs, node)
		return nil
	}
	if gate == depBlocked {
		logger.V(1).Info("node blocked — dependency in error state", "node", node.ID)
		plan.SetState(node.ID, NodeBlocked)
		return nil
	}
	if gate == depPending {
		logger.V(1).Info("node pending — dependency pending", "node", node.ID)
		plan.SetState(node.ID, NodePending)
		return nil
	}

	// --- propagateWhen input gate ---
	// For forEach nodes, propagateWhen is evaluated per-item inside
	// reconcileForEach, not here.
	if len(node.PropagateWhen) > 0 && node.ForEach == nil {
		// Populate soft dependencies as optional.none() before propagateWhen
		// evaluation. propagateWhen expressions may reference soft deps via
		// optional chaining (e.g. source.?data), which requires the variable
		// to exist in scope as an optional value.
		for depID, kind := range node.Dependencies {
			if kind != graphpkg.DepSoft {
				continue
			}
			if _, exists := eval.scope[depID]; exists {
				continue
			}
			eval.scope[depID] = celOptionalNone()
		}

		// Populate __kroDeps for this node so that <id>.dependencies()
		// expressions resolve correctly.
		populateDepsMap(eval, node)

		gate, requeueHint := eval.checkPropagateWhen(node.PropagateWhen, node.ID)
		if gate != gatePass {
			unsatisfied := eval.firstUnsatisfiedCondition(node.PropagateWhen)
			logger.V(1).Info("propagateWhen unsatisfied — node pending",
				"node", node.ID, "unsatisfied", unsatisfied)
			plan.SetState(node.ID, NodePending)
			// Use eval's accumulated hint as fallback (from accumulateTimeHint
			// during gate evaluation).
			if requeueHint == 0 {
				requeueHint = eval.requeueHint
			}
			// Return a synthetic result carrying just the requeue hint.
			if requeueHint > 0 {
				return &nodeIntegrationResult{requeueHint: requeueHint}
			}
			return nil
		}
	}

	// --- includeWhen ---
	if len(node.IncludeWhen) > 0 {
		included, err := eval.includeWhen(node.IncludeWhen)
		if err != nil {
			if errors.Is(err, ErrPending) {
				plan.SetState(node.ID, NodePending)
			} else {
				plan.SetState(node.ID, NodeError)
			}
			return nil
		}
		if !included {
			logger.V(1).Info("node excluded by includeWhen", "node", node.ID)
			plan.SetState(node.ID, NodeExcluded)
			markExcluded(eval, node.ID, state)
			removeMetricIfExcluded(rs, node)
			return nil
		}
	}

	// --- Evaluate the node ---
	cluster := r.cluster()
	nr := evaluateNode(ctx, cluster, rs, *node, eval, state)

	// --- Integrate the result into propagation state ---
	nrOut := integrateNodeResult(ctx, nr, node, eval, state, plan, nodeKeys)
	if nrOut.crdCreated && r.SchemaGen != nil {
		r.SchemaGen.AdvanceGeneration()
		nrOut.needsRecompile = true
	}
	return &nrOut
}

// nodeIntegrationResult carries the outputs of integrateNodeResult back
// to the propagation loop. Decouples the propagation from node-result classification
// internals (apiErrorInfo structure).
type nodeIntegrationResult struct {
	errMsgs        []string // "nodeID: reason" error messages for status
	needsRecompile bool     // dynamic GVK resolved or changed
	crdCreated     bool     // template node created a CRD (caller advances SchemaGen)
	requeueHint    time.Duration // time comparison solving hint (0 = none)
}

// integrateNodeResult processes a single node's evaluation output and
// updates the propagation's shared state: plan states, scope, keys, forEach,
// and dynamic GVK resolution. Handles error states (Error, Conflict,
// Pending) and the success path (scope publish, readiness merge, key
// recording). Returns status messages and signals for the caller.
//
// This is a pure integration function — it reads the nodeResult and
// mutates eval, state, plan, and nodeKeys. The CRD detection signal
// is returned rather than acted on, keeping SchemaGeneration coupling
// out of this function.
func integrateNodeResult(
	ctx context.Context,
	nr nodeResult,
	node *graphpkg.Node,
	eval *evaluator,
	state *instanceState,
	plan *PlanState,
	nodeKeys map[string][]Applied,
) nodeIntegrationResult {
	logger := log.FromContext(ctx)
	var out nodeIntegrationResult

	// --- Error states: set plan state, record error ---
	if nr.state == NodeError {
		info := classifyAPIError(nr.err)
		plan.SetState(node.ID, info.state)
		out.errMsgs = append(out.errMsgs, fmt.Sprintf("%s: %s", node.ID, info.reason))
		logger.V(0).Info("error on node", "node", node.ID, "state", info.state, "reason", info.reason, "error", nr.err)
		return out
	}
	if nr.state == NodeConflict {
		plan.SetState(node.ID, NodeConflict)
		out.errMsgs = append(out.errMsgs, fmt.Sprintf("%s: field conflict", node.ID))
		logger.V(0).Info("conflict on node", "node", node.ID, "error", nr.err)
		return out
	}
	if nr.state == NodePending {
		plan.SetState(node.ID, NodePending)
		logger.V(1).Info("data pending for node", "node", node.ID, "error", nr.err)
		return out
	}

	// --- Success path ---

	// Publish scope.
	if nr.scope != nil {
		eval.scope[node.ID] = nr.scope
	}

	// CRD creation detection: if a template node just created a CRD,
	// signal the caller to advance the schema generation so the post-walk
	// recompile check catches child graph type errors within the same cycle.
	if node.Type() == graphpkg.NodeTypeTemplate {
		if scopeMap, ok := nr.scope.(map[string]any); ok {
			if scopeMap["apiVersion"] == "apiextensions.k8s.io/v1" && scopeMap["kind"] == "CustomResourceDefinition" {
				out.crdCreated = true
			}
		}
	}

	// Merge node-readiness verdict.
	if eval.nodeReady != nil {
		eval.nodeReady[node.ID] = (nr.state == NodeReady)
	}

	// Update plan state.
	plan.SetState(node.ID, nr.state)
	if nr.state == NodeNotReady && nr.err != nil && errors.Is(nr.err, ErrReadyWhenFailed) {
		out.errMsgs = append(out.errMsgs, fmt.Sprintf("%s: %s", node.ID, nr.err.Error()))
		logger.V(0).Info("readyWhen expression error (not gating dependents)",
			"node", node.ID, "error", nr.err)
	}

	// Record applied keys.
	if nr.state == NodeReady || nr.state == NodeNotReady {
		nodeKeys[node.ID] = nr.keys
	}

	// Dynamic GVK resolution: signal recompile needed if a dynamic GVK
	// was resolved (schema was not available at compile time).
	if nr.resolvedGVK != nil {
		out.needsRecompile = true
		logger.Info("dynamic GVK resolved; will recompile with schema on next reconcile",
			"node", node.ID, "gvk", *nr.resolvedGVK)
	}

	// Merge evaluator-accumulated time hints from value expressions.
	if eval.requeueHint > 0 && (out.requeueHint == 0 || eval.requeueHint < out.requeueHint) {
		out.requeueHint = eval.requeueHint
	}

	return out
}

// evaluateNode runs reconcileNode for a single node and translates the
// result into a nodeResult. This is the evaluation boundary — all node-type
// dispatch (Definition, Template, Patch, Ref, Watch, ForEach) happens
// inside reconcileNode.
//
// The evaluator is used directly (no snapshot) since the propagation is sequential.
// Soft dependencies are populated as optional values before dispatch.
func evaluateNode(ctx context.Context, c *clusterAccess, rs *reconcileScope, node graphpkg.Node, eval *evaluator, state *instanceState) nodeResult {
	// Populate soft dependencies as CEL optional values. Per 005-reconciliation.md:
	// "Soft dependencies are always in scope as optional values."
	for depID, kind := range node.Dependencies {
		if kind != graphpkg.DepSoft {
			continue
		}
		if _, exists := eval.scope[depID]; exists {
			continue // already in scope from a previous node evaluation
		}
		// Soft dep not yet in scope — set optional.none() so the
		// expression's branch that handles absent data can fire.
		eval.scope[depID] = celOptionalNone()
	}

	out, err := reconcileNode(ctx, c, rs, node, eval)

	nr := nodeResult{
		state: NodeReady,
		scope: eval.scope[node.ID],
	}

	// Unpack nodeOutput fields.
	if out != nil {
		nr.keys = out.keys
	}

	if err != nil {
		nr.err = err
		switch {
		case errors.Is(err, ErrPending):
			nr.state = NodePending
		case errors.Is(err, ErrWaitingForReadiness):
			nr.state = NodeNotReady
		case errors.Is(err, ErrReadyWhenFailed):
			// readyWhen is a health signal — does not gate dependents.
			nr.state = NodeNotReady
		case errors.Is(err, ErrFieldConflict):
			nr.state = NodeConflict
		default:
			nr.state = NodeError
		}
	}

	// Dynamic GVK resolution: check if reconcileApply stored a resolved GVK.
	// In the simplified model, the evaluator tracks this per-node.
	if node.HasDynamicGVR() {
		if scopeMap, ok := nr.scope.(map[string]any); ok {
			gvk := graphpkg.GVKFromMap(scopeMap)
			if gvk.Kind != "" {
				nr.resolvedGVK = &gvk
			}
		}
	}

	return nr
}

// depGateOutcome represents the dependency gating outcome for a node.
type depGateOutcome int

const (
	_           depGateOutcome = iota // zero value unused
	depExcluded                       // at least one hard dep is Excluded
	depBlocked                        // at least one hard dep is in an error state
	depPending                        // at least one hard dep is Pending
)

// checkDependencyGate inspects a node's hard dependencies and determines
// whether the node can be dispatched for evaluation. Lazy dependencies
// are ignored — they don't gate dispatch.
//
// Precedence: Excluded > Blocked > Pending, matching
// 005-reconciliation.md § Propagation.
func checkDependencyGate(node *graphpkg.Node, plan *PlanState) depGateOutcome {
	hasExcluded := false
	hasBlocked := false
	hasPending := false
	for depID, kind := range node.Dependencies {
		if kind != graphpkg.DepHard {
			continue
		}
		depState, exists := plan.States[depID]
		if !exists {
			continue
		}
		switch depState {
		case NodeReady, NodeNotReady:
			// Dependency completed — proceed.
		case NodeExcluded:
			hasExcluded = true
		case NodePending:
			hasPending = true
		default:
			// Error, SystemError, Conflict, Blocked → blocked.
			hasBlocked = true
		}
	}
	switch {
	case hasExcluded:
		return depExcluded
	case hasBlocked:
		return depBlocked
	case hasPending:
		return depPending
	default:
		return 0 // all hard deps completed — proceed to evaluation
	}
}

// populateDepsMap injects the __kroDeps map entry for a node so that
// <id>.dependencies() CEL expressions resolve correctly.
func populateDepsMap(eval *evaluator, node *graphpkg.Node) {
	depsMap, _ := eval.scope[compiler.ReservedDepsMapVar].(map[string]any)
	if depsMap == nil {
		depsMap = make(map[string]any, 8)
		eval.scope[compiler.ReservedDepsMapVar] = depsMap
	}
	depValues := make([]any, 0, len(node.Dependencies))
	for depID := range node.Dependencies {
		if v, ok := eval.scope[depID]; ok {
			depValues = append(depValues, v)
		}
	}
	depsMap[node.ID] = depValues
}

// celOptionalOf wraps a scope value in a CEL optional.of() so lazy
// dependencies are available to expressions as optional values.
func celOptionalOf(v any) any {
	return types.OptionalOf(types.DefaultTypeAdapter.NativeToValue(v))
}

// celOptionalNone returns a CEL optional.none() for absent lazy dependencies.
func celOptionalNone() any {
	return types.OptionalNone
}

// markExcluded stamps a node as excluded in scope.
// Excluded nodes advertise ready=true so that non-excluded status rollup
// expressions don't block on absent nodes.
func markExcluded(eval *evaluator, nodeID string, state *instanceState) {
	eval.scope[nodeID] = map[string]any{"__ready": true}
	if eval.nodeReady != nil {
		eval.nodeReady[nodeID] = true
	}
}

// removeMetricIfExcluded removes a metric node's prometheus metric when the
// node becomes excluded (includeWhen false or contagious exclusion). Without
// this, an excluded metric retains its last-emitted value from when it was active.
func removeMetricIfExcluded(rs *reconcileScope, node *graphpkg.Node) {
	if node.Type() == graphpkg.NodeTypeMetric && node.Metric != nil && rs.metricStore != nil {
		rs.metricStore.Remove(rs.graphKey, node.Metric.Name)
	}
}
