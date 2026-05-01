// instance.go holds per-Graph mutable reconcile-time state.
//
// Each Graph CR gets its own instanceState even when sharing a
// compiledGraph with other instances. This is correct because the mutable
// state tracks per-instance Kubernetes resources with different observed
// states.
package graphcontroller

import (
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
	graphpkg "github.com/ellistarn/kro/experimental/controller/graph"
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
	spec *graphpkg.GraphSpec
	dag  *dagpkg.DAG

	// State retained across reconciles for prune diffing and state
	// carry-forward on skip.
	previousScope      map[string]any          // node ID → last scope data
	previousKeys       map[string][]string     // node ID → applied keys from last reconcile
	previousPlanStates *dagpkg.PlanState       // node states from last reconcile

	// forEach collection state retained across reconciles for prune key tracking.
	forEachItems     map[string][]any              // node ID → cached collection items
	forEachItemScope map[string]map[string]any     // nodeID → itemID → scope data
	forEachItemKeys  map[string]map[string][]string // nodeID → itemID → applied keys

	// Previous applied key set — used to detect intra-revision prune need
	// via deriveAppliedSet on cold start.
	previousAppliedKeys map[string]bool

	// deferredPruneKeys carries keys whose deletion was deferred in the last
	// reconcile (finalization in progress, third-party field managers, etc.).
	// These are injected into allPreviousKeys on the next reconcile so they
	// remain visible as prune candidates regardless of watch-cache lag.
	deferredPruneKeys []string

	// previousPruneNotes carries informational notes (e.g., FinalizerSkipped)
	// from the last prune cycle. Surfaced in status for one additional
	// reconcile after the prune completes so the transient note is observable.
	previousPruneNotes []string

	// activeFinalization tracks in-flight finalization sequences. Maps the
	// target resource key → finalization state (phase + child keys). The prune
	// walk consults this before deleting anything: keys that appear as child
	// keys are protected from pruning. Cleared when the target is successfully
	// deleted (finalization complete). Persists across reconciles so
	// subsequent prune cycles don't race with in-progress finalization.
	activeFinalization map[string]*finalizationEntry

	// resolvedDynamicGVKs maps node ID → last-resolved GVK for dynamic GVK nodes.
	// Per 004-compilation.md § Deferred Types: "When the reconciler evaluates
	// a dynamic GVK expression and gets a different type than what was compiled
	// against, that node's compilation is stale." The reconciler updates this
	// map after evaluating each dynamic GVK node and compares it on subsequent
	// reconciles to detect staleness.
	resolvedDynamicGVKs map[string]schema.GroupVersionKind
}

// newInstanceState creates a fresh instanceState for a compiledGraph.
func newInstanceState(compiled *compiler.CompiledGraph) *instanceState {
	return &instanceState{
		compiled:         compiled,
		previousScope:    make(map[string]any),
		previousKeys:     make(map[string][]string),
		forEachItems:     make(map[string][]any),
		forEachItemScope: make(map[string]map[string]any),
		forEachItemKeys:  make(map[string]map[string][]string),
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
