// instance.go holds per-Graph mutable reconcile-time state.
//
// Each Graph CR gets its own instanceState even when sharing a
// compiledGraph with other instances. This is correct because the mutable
// state tracks per-instance Kubernetes resources with different observed
// states.
package graphcontroller

import (
	"math/rand"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kubernetes-sigs/kro/experimental/controller/compiler"
	dagpkg "github.com/kubernetes-sigs/kro/experimental/controller/dag"
	"github.com/kubernetes-sigs/kro/experimental/controller/graph"
)

// instanceState holds the mutable reconcile-time state for a single Graph
// instance. Each Graph CR gets its own instanceState even when sharing a
// compiledGraph with other instances. This is correct because the mutable
// state tracks per-instance Kubernetes resources with different observed states.
type instanceState struct {
	compiled *compiler.CompiledGraph

	// Per-instance spec and DAG. The compiled graph is shared across instances
	// with the same compilation key; the spec and DAG contain per-instance
	// node bodies (template maps with concrete values).
	// Per 004-compilation.md § Structural Compilation Caching.
	spec *graph.GraphSpec
	dag  *dagpkg.DAG

	// State retained across reconciles for propagateWhen and forEach diffing.
	previousScope      map[string]any       // node ID → last scope data
	previousKeys       map[string][]string  // node ID → last applied keys
	previousPlanStates map[string]dagpkg.NodeState // node ID → last plan state
	forEachItems       map[string][]any     // "nodeID/varName" → cached collection items

	// Evaluation hashing state — retained across reconciles for change detection.
	// See 005-reconciliation.md § Propagation.
	previousEvalHashes map[string]string // node ID → last dependency evaluation hash
	previousSelfHashes map[string]string // node ID → last self-section hash

	// Per-node forEach item state. Outer key is node ID, inner key is item identity.
	// Structural boundary prevents prefix collisions between node IDs.
	forEachItemScope  map[string]map[string]any      // nodeID → itemID → scope data
	forEachItemKeys   map[string]map[string][]string // nodeID → itemID → applied keys
	forEachItemHashes map[string]map[string]string   // nodeID → itemID → content hash

	// Previous applied key set — used to detect intra-revision prune need.
	// When the key set changes between reconciles (forEach scale-down,
	// includeWhen toggle), the watch cache scan runs to find prune candidates.
	// Steady-state reconciles (same key set) skip the scan entirely.
	previousAppliedKeys map[string]bool

	// deferredPruneKeys carries keys whose deletion was deferred in the last
	// reconcile (finalization in progress, third-party field managers, etc.).
	// These are injected into allPreviousKeys on the next reconcile so they
	// remain visible as prune candidates regardless of watch-cache lag.
	// Distinct from previousAppliedKeys: those track what was applied (for
	// prune diffing); these track what is pending deletion (for retry).
	deferredPruneKeys map[string]bool

	// activeFinalization tracks in-flight finalization sequences. Maps the
	// target resource key → finalization state (phase + child keys). The prune
	// walk consults this before deleting anything: keys that appear as child
	// keys are protected from pruning. Cleared when the target is successfully
	// deleted (finalization complete). Persists across reconciles so
	// subsequent prune cycles don't race with in-progress finalization.
	activeFinalization map[string]*finalizationEntry

	// Per-node resync timer expiry times. When expired, the node is triggered
	// unconditionally — the consistency floor. Reset on successful evaluation.
	// On restart, all timers start fresh with random jitter.
	// Per 005-reconciliation.md § Reconcile.
	resyncTimers map[string]time.Time

	// resolvedDynamicGVKs maps node ID → last-resolved GVK for dynamic GVK nodes.
	// Per 004-compilation.md § Deferred Types: "When the reconciler evaluates
	// a dynamic GVK expression and gets a different type than what was compiled
	// against, that node's compilation is stale." The reconciler updates this
	// map after evaluating each dynamic GVK node and compares it on subsequent
	// reconciles to detect staleness.
	resolvedDynamicGVKs map[string]schema.GroupVersionKind

	// collectionCache holds the cached full-object list per Watch node.
	// Per 005-reconciliation.md § Propagation: "When a single resource
	// changes, update the cached list incrementally rather than re-listing
	// — O(1) per event, not O(matching)." On watch events, only the changed
	// items are GET'd and merged. On resync timer, the cache is replaced via
	// full List.
	collectionCache map[string][]any // node ID → cached collection items

	// collectionDirty tracks Watch nodes whose previous incremental
	// reconcile failed mid-merge. When the incremental path returns an
	// error (e.g., a transient 5xx on one of the GETs), drained
	// CollectionChanges are lost — the next reconcile would take the
	// incremental path with stale cache. Setting this flag forces a full
	// re-List on the next reconcile so the cache recovers from the
	// authoritative API server state. Cleared when the next successful
	// evaluation writes collectionUpdatedCache.
	collectionDirty map[string]bool

	// nodeReady persists per-Watch readyWhen verdicts across
	// reconciles. The AST rewrite of `<wk_id>.ready()` looks up this
	// map via the reserved scope variable; a Watch that isn't
	// re-evaluated on a given reconcile must still expose its last
	// verdict, otherwise downstream `.ready()` errors with
	// "no such key" and the consumer incorrectly transitions out of
	// Ready. Per 001-graph.md § readyWhen.
	nodeReady map[string]bool

	// systemErrorBackoff tracks the current exponential backoff duration per
	// node in SystemError state. Per 005-reconciliation.md § Trigger:
	// "Transient errors (5xx) retry with exponential backoff [1s,
	// resyncInterval]." Doubles on each consecutive SystemError, resets on
	// any non-SystemError state.
	systemErrorBackoff map[string]time.Duration
}

// newInstanceState creates a fresh instanceState for a compiledGraph.
func newInstanceState(compiled *compiler.CompiledGraph) *instanceState {
	return &instanceState{
		compiled:           compiled,
		previousScope:      map[string]any{},
		previousKeys:       map[string][]string{},
		previousPlanStates: map[string]dagpkg.NodeState{},
		previousEvalHashes: map[string]string{},
		previousSelfHashes: map[string]string{},
		forEachItems:       map[string][]any{},
		forEachItemScope:   map[string]map[string]any{},
		forEachItemKeys:    map[string]map[string][]string{},
		forEachItemHashes:  map[string]map[string]string{},
		resyncTimers:       make(map[string]time.Time),
		collectionCache:    make(map[string][]any),
		collectionDirty:    make(map[string]bool),
		nodeReady:          make(map[string]bool),
		systemErrorBackoff: make(map[string]time.Duration),
	}
}

// updateAppliedKeys stores the current key set as the comparison baseline.
// Call this after prune completes successfully.
func (s *instanceState) updateAppliedKeys(keys []string) {
	s.previousAppliedKeys = make(map[string]bool, len(keys))
	for _, k := range keys {
		s.previousAppliedKeys[k] = true
	}
}

// resetResyncTimer sets the resync timer for a node to fire after the default
// interval plus jitter. Called after a node is successfully evaluated.
// Per 005-reconciliation.md § Reconcile: "An SSA apply resets the resync timer."
func (s *instanceState) resetResyncTimer(nodeID string, interval, maxJitter time.Duration) {
	var jitter time.Duration
	if maxJitter > 0 {
		jitter = time.Duration(rand.Int63n(int64(maxJitter)))
	}
	s.resyncTimers[nodeID] = time.Now().Add(interval + jitter)
}

// isResyncExpired reports whether a node's resync timer has expired.
// Returns false if no timer is set (first reconcile handles this via
// the "all nodes triggered" path).
func (s *instanceState) isResyncExpired(nodeID string) bool {
	expiry, ok := s.resyncTimers[nodeID]
	if !ok {
		return false
	}
	return time.Now().After(expiry)
}

// nextResyncExpiry returns the earliest resync timer expiry across all nodes.
// Returns zero time if no timers are set. Used to schedule the next
// reconcile via RequeueAfter — the consistency floor.
func (s *instanceState) nextResyncExpiry() time.Time {
	var earliest time.Time
	for _, expiry := range s.resyncTimers {
		if earliest.IsZero() || expiry.Before(earliest) {
			earliest = expiry
		}
	}
	return earliest
}

// mergeDynamicGVK records a resolved dynamic GVK for a node and reports
// whether the compilation key has changed (first resolution or GVK change).
// When stale=true, the caller should requeue: the next reconcile will compute
// a new compilation key that includes the resolved GVK, causing a cache miss
// and recompilation with the schema available for type checking.
func (s *instanceState) mergeDynamicGVK(nodeID string, resolved schema.GroupVersionKind) (stale bool) {
	if s.resolvedDynamicGVKs == nil {
		s.resolvedDynamicGVKs = make(map[string]schema.GroupVersionKind)
	}
	prevGVK, hadPrev := s.resolvedDynamicGVKs[nodeID]
	stale = !hadPrev || prevGVK != resolved
	s.resolvedDynamicGVKs[nodeID] = resolved
	return stale
}
