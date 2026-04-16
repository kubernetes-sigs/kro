// cel.go contains CEL runtime integration: expression compilation/evaluation,
// custom CEL functions (plural, simpleSchema.toOpenAPI), and error classification
// for distinguishing retryable "data pending" errors from expression bugs.
//
// Performance model: CEL environments and programs are compiled eagerly when a
// Graph spec is first seen (or when it changes). The reconcile loop only evaluates
// pre-compiled programs — no compilation happens during the resource walk.
//
// Compiled graph sharing: compiled artifacts (CEL env, programs, DAG) are
// content-addressed by spec hash. Multiple Graph instances with identical specs
// (e.g., nested graphs stamped by forEach) share a single compiledGraph. Per-instance
// mutable state (scope, input hashes, forEach state) is tracked separately in
// instanceState, keyed by namespace/revision-name.
package graphcontroller

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"

	"github.com/gobuffalo/flect"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/conversion"
	"github.com/kubernetes-sigs/kro/pkg/simpleschema"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ErrPending indicates that CEL evaluation failed because required data
// is not yet available (e.g., a resource's status field hasn't been populated).
// This is a retryable condition — the controller should requeue and try again.
var ErrPending = errors.New("data pending")

// ErrEvaluation indicates that the error originates from a non-API operation:
// CEL evaluation, template rendering, JSON marshaling, or other deterministic
// local computation. Errors wrapped with this sentinel are classified as
// NodeError by classifyAPIError, even if their message text happens to contain
// network-like patterns (e.g., "unexpected EOF" from malformed JSON). Without
// this, the string-based network error pattern matcher would misclassify them
// as NodeSystemError, triggering 5-second retry for errors that can only
// resolve via propagation or spec change.
var ErrEvaluation = errors.New("evaluation error")

// ErrWaitingForReadiness indicates that a resource exists but hasn't satisfied
// its readyWhen conditions yet. Downstream resources should wait.
var ErrWaitingForReadiness = errors.New("waiting for readiness")

// ErrInvalidExpression indicates that one or more CEL expressions in the Graph
// spec failed to compile. This is a permanent error until the spec is fixed.
var ErrInvalidExpression = errors.New("invalid expression")

// ErrFieldConflict indicates that an SSA apply received a 409 Conflict because
// another actor has taken ownership of fields the controller manages. This is
// a permanent error for the resource until the external actor releases the
// field or the Graph spec changes to no longer write that field.
var ErrFieldConflict = errors.New("field conflict")

// ErrReadyWhenFailed indicates that a readyWhen expression failed to evaluate
// due to a permanent expression error (not data pending, not a transient
// condition). Per 001-graph.md: "readyWhen is a health signal — it does not
// gate downstream execution." The coordinator classifies this as NodeNotReady
// (not NodeError) so dependents proceed. The underlying error is preserved in
// the chain for logging and status reporting.
var ErrReadyWhenFailed = errors.New("readyWhen evaluation failed")

// ErrDependencyError indicates that the dependency graph contains a cycle.
// This is a permanent error until the spec is fixed.
var ErrDependencyError = errors.New("circular dependency")

// celPendingPatterns are CEL error patterns that indicate data is not yet
// available (retryable). Other CEL errors are considered expression bugs.
//
// Data pending (retryable):
//   - "no such key"        : map key doesn't exist (e.g., status.field not populated)
//   - "no such field"      : struct field doesn't exist yet
//   - "no such attribute"  : dependency resource not yet in context
//   - "index out of bounds": list doesn't have enough items yet
//
// NOT data pending (expression bugs):
//   - "type conversion error" : wrong types in expression
//   - "no such overload"      : invalid operation for types
//   - "division by zero"      : math error
var celPendingPatterns = []string{
	"no such key",
	"no such field",
	"no such attribute",
	"index out of bounds",
}

// isCELPending checks if a CEL error indicates data is pending.
func isCELPending(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, pattern := range celPendingPatterns {
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}

// isPending checks if an error indicates data is pending — either a CEL
// runtime error (string pattern match) or a wrapped ErrPending sentinel.
func isPending(err error) bool {
	return isCELPending(err) || errors.Is(err, ErrPending)
}

// ---------------------------------------------------------------------------
// Compiled graph (immutable, shareable, content-addressed)
// ---------------------------------------------------------------------------

// compiledGraph holds the immutable compilation artifacts for a Graph spec.
// All fields are derived from the spec and are safe to share across multiple
// Graph instances with identical specs (e.g., nested graphs stamped by forEach).
//
// Content-addressed by specHash: two Graph specs that produce the same hash
// share a single compiledGraph. The DAG, CEL programs, and CEL environment are
// all immutable after construction — cel.Program is thread-safe by the CEL spec,
// and BuildDAG produces a read-only structure (verified: zero writes to DAG
// fields during reconciliation).
type compiledGraph struct {
	specHash       string                            // content hash of the compilation inputs
	env            *cel.Env                          // CEL environment (immutable after Extend)
	programs       map[string]cel.Program            // expression string → compiled program
	exprPaths      map[string]map[string][]FieldPath // expression string → (scope var → field paths)
	declaredVars   map[string]bool                   // variable names declared in the CEL env
	spec           *GraphSpec                        // parsed spec (immutable)
	dag            *DAG                              // dependency graph (immutable after BuildDAG)
	unresolvedGVKs []schema.GroupVersionKind         // GVKs that fell back to dyn (triggers recompilation on CRD install)
}

// eval evaluates a CEL expression against the given scope.
// First checks the pre-compiled program cache; if the expression is not found
// (e.g., a readyWhen expression from a superseded revision evaluated using the
// current revision's evaluator), it compiles the expression on-the-fly using
// the current CEL environment. This handles cross-revision finalization where
// the snapshot's readyWhen may reference nodes declared in the current spec but
// whose expression was only pre-compiled in the old spec.
func (c *compiledGraph) eval(expr string, scope map[string]any) (any, error) {
	prg, ok := c.programs[expr]
	if !ok {
		// Expected cache miss during cross-revision finalization or forEach
		// finalization. The expression may reference variables (node IDs,
		// forEach iterator variables) that aren't declared in the current
		// revision's CEL env. Extend the env with scope keys not already
		// declared so the compiler can resolve them.
		var varDecls []cel.EnvOption
		for k := range scope {
			if !c.declaredVars[k] {
				varDecls = append(varDecls, cel.Variable(k, cel.DynType))
			}
		}
		compileEnv := c.env
		if len(varDecls) > 0 {
			dynEnv, extErr := c.env.Extend(varDecls...)
			if extErr != nil {
				return nil, fmt.Errorf("expression %q: extending CEL env for dynamic compile: %w", expr, extErr)
			}
			compileEnv = dynEnv
		}
		ast, issues := compileEnv.Compile(expr)
		if issues != nil && issues.Err() != nil {
			return nil, fmt.Errorf("expression %q not in cache, dynamic compile failed: %w", expr, issues.Err())
		}
		var err error
		prg, err = compileEnv.Program(ast)
		if err != nil {
			return nil, fmt.Errorf("expression %q not in cache, dynamic program failed: %w", expr, err)
		}
	}

	out, _, err := prg.Eval(scope)
	if err != nil {
		if isCELPending(err) {
			return nil, fmt.Errorf("evaluating %q: %w: %w", expr, ErrPending, err)
		}
		return nil, fmt.Errorf("evaluating %q: %w", expr, err)
	}

	native, err := conversion.GoNativeType(out)
	if err != nil {
		return nil, fmt.Errorf("converting %q result: %w", expr, err)
	}

	return native, nil
}

// ---------------------------------------------------------------------------
// Instance state (mutable, per-graph-instance)
// ---------------------------------------------------------------------------

// instanceState holds the mutable reconcile-time state for a single Graph
// instance. Each Graph CR gets its own instanceState even when sharing a
// compiledGraph with other instances. This is correct because the mutable
// state tracks per-instance Kubernetes resources with different observed states.
type instanceState struct {
	compiled *compiledGraph

	// State retained across reconciles for propagateWhen and forEach diffing.
	previousScope          map[string]any       // node ID → last scope data
	previousKeys           map[string][]string  // node ID → last applied keys
	previousPlanStates     map[string]NodeState // node ID → last plan state
	previousPropagateReady map[string]bool      // node ID → last propagateWhen result (for forEach skip path)
	forEachItems           map[string][]any     // "nodeID/varName" → cached collection items

	// Resolved references for Unresolved nodes. Set on first reconcile when the
	// existence check determines Own vs Contribute. Persists across
	// reconciles within the same revision. Reset on new revision (new
	// instanceState).
	resolvedReferences map[string]Reference

	// Evaluation hashing state — retained across reconciles for change detection.
	// See 004-graph-reconciliation.md § Propagation.
	previousEvalHashes map[string]string // node ID → last dependency evaluation hash
	previousSelfHashes map[string]string // node ID → last self-section hash

	// Per-node forEach item state. Outer key is node ID, inner key is item identity.
	// Structural boundary prevents prefix collisions between node IDs.
	forEachItemScope map[string]map[string]any      // nodeID → itemID → scope data
	forEachItemKeys  map[string]map[string][]string // nodeID → itemID → applied keys

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

	// Per-node drift timer expiry times. When expired, the node is triggered
	// unconditionally — the consistency floor. Reset on successful evaluation.
	// On restart, all timers start fresh with random jitter.
	// Per 004-graph-reconciliation.md § Reconcile.
	driftTimers map[string]time.Time

	// watchKindCache holds the cached full-object list per WatchKind node.
	// Per 004-graph-reconciliation.md § Propagation: "When a single resource
	// changes, update the cached list incrementally rather than re-listing
	// — O(1) per event, not O(matching)." On watch events, only the changed
	// items are GET'd and merged. On drift timer, the cache is replaced via
	// full List.
	watchKindCache map[string][]any // node ID → cached collection items

	// systemErrorBackoff tracks the current exponential backoff duration per
	// node in SystemError state. Per 004-graph-reconciliation.md § Trigger:
	// "Transient errors (5xx) retry with exponential backoff [1s,
	// resyncInterval]." Doubles on each consecutive SystemError, resets on
	// any non-SystemError state.
	systemErrorBackoff map[string]time.Duration
}

// newInstanceState creates a fresh instanceState for a compiledGraph.
func newInstanceState(compiled *compiledGraph) *instanceState {
	return &instanceState{
		compiled:               compiled,
		previousScope:          map[string]any{},
		previousKeys:           map[string][]string{},
		previousPlanStates:     map[string]NodeState{},
		previousPropagateReady: map[string]bool{},
		previousEvalHashes:     map[string]string{},
		previousSelfHashes:     map[string]string{},
		forEachItems:           map[string][]any{},
		forEachItemScope:       map[string]map[string]any{},
		forEachItemKeys:        map[string]map[string][]string{},
		resolvedReferences:     make(map[string]Reference, len(compiled.dag.References)),
		driftTimers:            make(map[string]time.Time),
		watchKindCache:         make(map[string][]any),
		systemErrorBackoff:     make(map[string]time.Duration),
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

// resetDriftTimer sets the drift timer for a node to fire after the default
// interval plus jitter. Called after a node is successfully evaluated.
// Per 004-graph-reconciliation.md § Reconcile: "An SSA apply resets the drift timer."
func (s *instanceState) resetDriftTimer(nodeID string, interval, maxJitter time.Duration) {
	var jitter time.Duration
	if maxJitter > 0 {
		jitter = time.Duration(rand.Int63n(int64(maxJitter)))
	}
	s.driftTimers[nodeID] = time.Now().Add(interval + jitter)
}

// isDriftExpired reports whether a node's drift timer has expired.
// Returns false if no timer is set (first reconcile handles this via
// the "all nodes triggered" path).
func (s *instanceState) isDriftExpired(nodeID string) bool {
	expiry, ok := s.driftTimers[nodeID]
	if !ok {
		return false
	}
	return time.Now().After(expiry)
}

// nextDriftExpiry returns the earliest drift timer expiry across all nodes.
// Returns zero time if no timers are set. Used to schedule the next
// reconcile via RequeueAfter — the consistency floor.
func (s *instanceState) nextDriftExpiry() time.Time {
	var earliest time.Time
	for _, expiry := range s.driftTimers {
		if earliest.IsZero() || expiry.Before(earliest) {
			earliest = expiry
		}
	}
	return earliest
}

// initResolvedReferences seeds the resolved references map from the DAG's
// compile-time references. Called once at the start of each reconcile to
// ensure all nodes have an entry. Unresolved entries will be resolved during
// the walk.
func (s *instanceState) initResolvedReferences() {
	for id, ref := range s.compiled.dag.References {
		if _, ok := s.resolvedReferences[id]; !ok {
			s.resolvedReferences[id] = ref
		}
	}
}

// ---------------------------------------------------------------------------
// Cache management
// ---------------------------------------------------------------------------

// graphCaches manages two cache layers:
//   - compiled: content-addressed compiledGraph instances shared across all
//     Graph instances with identical specs. Keyed by spec hash.
//   - instances: per-Graph-instance mutable state. Keyed by namespace/revision-name.
//
// This separation means N identical child graphs (common in nested graph
// patterns with forEach) share one compiledGraph instead of each independently
// compiling identical CEL programs and DAGs.
type graphCaches struct {
	// RWMutex: get and getCompiled are read-only and take RLock; set and remove
	// mutate both maps and take Lock. The read path (one get per reconcile per
	// graph) is the hot path under multi-worker controllers.
	mu        sync.RWMutex
	compiled  map[string]*compiledGraph // spec hash → shared compiled graph
	instances map[string]*instanceState // namespace/revision-name → per-instance state

	// evicted holds instance states that were evicted by evictUnresolved().
	// When a type schema becomes available and triggers recompilation, the
	// node structure is unchanged — only type resolution improved. The
	// evicted state is used by compileRevision to migrate hashes and
	// references into the new instanceState, avoiding unnecessary re-evaluation
	// and re-application of unchanged nodes.
	evicted map[string]*instanceState
}

func newGraphCaches() *graphCaches {
	return &graphCaches{
		compiled:  make(map[string]*compiledGraph),
		instances: make(map[string]*instanceState),
		evicted:   make(map[string]*instanceState),
	}
}

func (gc *graphCaches) get(key string) *instanceState {
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	return gc.instances[key]
}

// getAny returns the instanceState for the given key, checking both the active
// instances map and the evicted map. Used during revision transitions to
// transfer previousAppliedKeys from a superseded revision whose state may
// have been evicted by evictUnresolved (schema discovery race).
//
// The returned pointer is safe to use after lock release because popEvicted
// (the only evicted-map removal path) runs on the reconciler goroutine, which
// is serialized with callers of getAny by the controller-runtime work queue.
// If this invariant changes, the returned pointer requires a defensive copy.
func (gc *graphCaches) getAny(key string) *instanceState {
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	if s := gc.instances[key]; s != nil {
		return s
	}
	return gc.evicted[key]
}

func (gc *graphCaches) set(key string, s *instanceState) {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	gc.instances[key] = s
	// Ensure the compiledGraph is also tracked.
	if s.compiled != nil {
		gc.compiled[s.compiled.specHash] = s.compiled
	}
}

func (gc *graphCaches) remove(key string) {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	inst := gc.instances[key]
	delete(gc.instances, key)

	// Sweep: if no other instance references this compiledGraph, remove it.
	// O(instances) per removal — acceptable for typical graph counts (<1000).
	// If this becomes hot (e.g., 10K+ forEach items tearing down), replace
	// with a reference count or reverse index from specHash → instance keys.
	if inst != nil && inst.compiled != nil {
		hash := inst.compiled.specHash
		referenced := false
		for _, other := range gc.instances {
			if other.compiled != nil && other.compiled.specHash == hash {
				referenced = true
				break
			}
		}
		if !referenced {
			delete(gc.compiled, hash)
		}
	}
}

// popEvicted returns and removes a stashed instanceState that was evicted by
// evictUnresolved for the given instance key. Returns nil if no evicted state
// exists. Used by compileRevision to migrate per-node state into the
// replacement instanceState after a type-resolution-triggered recompilation.
func (gc *graphCaches) popEvicted(key string) *instanceState {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	old := gc.evicted[key]
	delete(gc.evicted, key)
	return old
}

// getCompiled returns a shared compiledGraph by spec hash, or nil if not cached.
func (gc *graphCaches) getCompiled(specHash string) *compiledGraph {
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	return gc.compiled[specHash]
}

// CacheSizes returns the number of compiled graphs and instance states.
func (gc *graphCaches) CacheSizes() (compiledCount, instanceCount int) {
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	return len(gc.compiled), len(gc.instances)
}

// evictUnresolved removes compiled graphs that have unresolved GVKs and their
// instance states. Returns the affected Graph keys so callers can enqueue them
// for recompilation. Called when a CRD is installed — the previously-unresolvable
// schema may now be available.
func (gc *graphCaches) evictUnresolved() []graphKey {
	gc.mu.Lock()
	defer gc.mu.Unlock()

	// Find compiled graphs with unresolved GVKs.
	evictHashes := make(map[string]bool)
	for hash, compiled := range gc.compiled {
		if len(compiled.unresolvedGVKs) > 0 {
			evictHashes[hash] = true
			delete(gc.compiled, hash)
		}
	}
	if len(evictHashes) == 0 {
		return nil
	}

	// Evict instance states pointing to evicted compilations.
	// Stash them for state migration when the replacement instanceState is
	// created by compileRevision.
	// Extract Graph keys from instance keys (format: "namespace/revision-name").
	var affected []graphKey
	for key, state := range gc.instances {
		if state.compiled != nil && evictHashes[state.compiled.specHash] {
			gc.evicted[key] = state
			delete(gc.instances, key)
			// Instance key is "namespace/revision-name". The Graph name
			// is the revision name minus the "-gNNNNN" suffix.
			if gk, ok := instanceKeyToGraphKey(key); ok {
				affected = append(affected, gk)
			}
		}
	}
	return affected
}

// instanceKeyToGraphKey extracts a graphKey from an instance cache key.
// Instance keys are "namespace/graphname-gNNNNN". Returns false if the
// key doesn't match the expected format.
func instanceKeyToGraphKey(instanceKey string) (graphKey, bool) {
	// Split "namespace/revision-name"
	slash := strings.Index(instanceKey, "/")
	if slash < 0 {
		return graphKey{}, false
	}
	ns := instanceKey[:slash]
	revName := instanceKey[slash+1:]

	// Revision name format: "graphname-gNNNNN" — find the last "-g" followed by digits.
	lastDash := strings.LastIndex(revName, "-g")
	if lastDash < 0 || lastDash+2 >= len(revName) {
		return graphKey{}, false
	}
	// Verify the suffix after "-g" is all digits.
	suffix := revName[lastDash+2:]
	for _, c := range suffix {
		if c < '0' || c > '9' {
			return graphKey{}, false
		}
	}
	return graphKey{Name: revName[:lastDash], Namespace: ns}, true
}

// ---------------------------------------------------------------------------
// Compilation
// ---------------------------------------------------------------------------

// compileGraphSpec builds a typed CEL environment, eagerly compiles every
// expression, and builds the dependency graph. Returns a compiledGraph ready
// for sharing across multiple instances.
//
// The typeInfo parameter carries resolved types from resolveNodeTypes. When nil,
// all nodes fall back to dyn.
func compileGraphSpec(spec *GraphSpec, typeInfo *typeSource) (*compiledGraph, error) {
	if typeInfo == nil {
		// No type information — all nodes declared as dyn (backwards compat).
		typeInfo = resolveNodeTypes(spec.Nodes, nil)
	}

	// Phase 3: build the typed CEL environment.
	// All typed declarations (resource schemas + definition types) go through
	// a single DeclTypeProvider to avoid the double-provider problem.
	typedDecls := buildTypedEnvOptions(typeInfo)

	env, err := krocel.DefaultEnvironment(
		krocel.WithResourceIDs(typeInfo.untypedIDs),
		krocel.WithListVariables(typeInfo.listIDs),
		krocel.WithCustomDeclarations(typedDecls),
		krocel.WithCustomDeclarations(celPluralFunction()),
		krocel.WithCustomDeclarations(celSimpleSchemaFunction()),
		krocel.WithCustomDeclarations(celReadyFunction()),
	)
	if err != nil {
		return nil, fmt.Errorf("creating CEL env: %w", err)
	}

	// Phase 4: compile expressions and build DAG.
	allIDs := spec.AllIdentifiers()

	// Build scope var set for field path extraction.
	scopeVars := make(map[string]bool, len(allIDs))
	for _, id := range allIDs {
		scopeVars[id] = true
	}

	expressions := spec.AllExpressions()
	programs := make(map[string]cel.Program, len(expressions))
	exprPaths := make(map[string]map[string][]FieldPath, len(expressions))

	for _, expr := range expressions {
		ast, issues := env.Compile(expr)
		if issues != nil && issues.Err() != nil {
			return nil, fmt.Errorf("compiling expression %q: %w: %w", expr, ErrInvalidExpression, issues.Err())
		}
		// Extract field paths from the AST before creating the program.
		// Per 004-graph-reconciliation.md § Hash Mechanics: "At graph compilation,
		// the controller walks each compiled expression's AST to extract
		// reference chains." One walk per expression, at compile time.
		exprPaths[expr] = extractFieldPathsFromAST(ast.NativeRep().Expr(), scopeVars, nil)
		prg, err := env.Program(ast)
		if err != nil {
			return nil, fmt.Errorf("programming expression %q: %w: %w", expr, ErrInvalidExpression, err)
		}
		programs[expr] = prg
	}

	// Build the dependency graph using pre-extracted field paths.
	// Cycle detection happens here — a cycle in the dependency graph
	// sets Compiled=False with DependencyError reason.
	dag, err := BuildDAG(spec.Nodes, exprPaths)
	if err != nil {
		return nil, err
	}

	// Track which variable names are declared in the CEL env so that
	// dynamic compilation during finalization can extend the env with
	// only new variables (avoiding "overlapping identifier" errors).
	declared := make(map[string]bool, len(allIDs))
	for _, id := range allIDs {
		declared[id] = true
	}

	return &compiledGraph{
		specHash:       spec.Hash(),
		env:            env,
		programs:       programs,
		exprPaths:      exprPaths,
		declaredVars:   declared,
		spec:           spec,
		dag:            dag,
		unresolvedGVKs: typeInfo.unresolvedGVKs,
	}, nil
}

// ---------------------------------------------------------------------------
// Spec hashing
// ---------------------------------------------------------------------------

// Hash computes a deterministic content hash of the compilation inputs.
// Two GraphSpecs that produce the same hash will produce identical compiledGraphs
// (same CEL programs, same DAG, same expression set).
//
// The hash covers: node IDs, template structures, forEach definitions,
// includeWhen/readyWhen/propagateWhen conditions — everything that feeds into
// compileGraphSpec. If a new compilation input is added to GraphSpec without
// updating this hash, content-addressed sharing will silently reuse stale
// compiled graphs. The test TestSpecHashCoversCompilationInputs guards against this.
//
// Each field is length-prefixed (binary.LittleEndian int64) before its content
// to prevent delimiter injection / field boundary ambiguity. This is strictly
// correct — no two distinct specs can produce the same hash input sequence.
//
// Collision probability: FNV-64a has a 64-bit output. At 1000 distinct specs
// the collision probability is ~2.7e-14 (birthday bound). At 1M distinct specs
// it's ~2.7e-8. This is accepted as negligible for an in-memory optimization
// cache. A collision would cause two different specs to share a compiled graph,
// producing incorrect CEL evaluation. If this ever matters, upgrade to FNV-128
// or SHA-256 — the hash is not on the hot path.
//
// Implementation notes:
//   - encoding/json sorts map keys deterministically in Go, so json.Marshal
//     of map[string]any produces a canonical byte sequence.
//   - Nodes are processed in declaration order (spec order is stable from
//     Kubernetes API). IncludeWhen, ReadyWhen, PropagateWhen are slices
//     (order-stable from spec parsing). ForEach is a map (sorted explicitly).
func (s *GraphSpec) Hash() string {
	h := fnv.New64a()

	// hashField writes a length-prefixed field to the hash.
	// Length prefix eliminates field boundary ambiguity (the classic delimiter
	// injection problem) without requiring reserved separator bytes.
	hashField := func(data []byte) {
		binary.Write(h, binary.LittleEndian, int64(len(data))) //nolint:errcheck
		h.Write(data)
	}

	// Process nodes in declaration order (spec order is stable).
	for _, node := range s.Nodes {
		// Node ID
		hashField([]byte(node.ID))

		// Template (deterministic via json.Marshal sorted map keys)
		if node.Template != nil {
			data, _ := json.Marshal(node.Template)
			hashField(data)
		} else {
			hashField(nil)
		}

		// Finalizes
		hashField([]byte(node.Finalizes))

		// ForEach (sorted keys for determinism — ForEach is a map)
		if node.ForEach != nil {
			forEachKeys := make([]string, 0, len(node.ForEach))
			for k := range node.ForEach {
				forEachKeys = append(forEachKeys, k)
			}
			sort.Strings(forEachKeys)
			for _, k := range forEachKeys {
				hashField([]byte(k))
				hashField([]byte(node.ForEach[k]))
			}
		}
		// Write forEach count to distinguish nil from empty.
		binary.Write(h, binary.LittleEndian, int64(len(node.ForEach))) //nolint:errcheck

		// Conditions — slices, order-stable from spec parsing.
		for _, c := range node.IncludeWhen {
			hashField([]byte(c))
		}
		binary.Write(h, binary.LittleEndian, int64(len(node.IncludeWhen))) //nolint:errcheck

		for _, c := range node.ReadyWhen {
			hashField([]byte(c))
		}
		binary.Write(h, binary.LittleEndian, int64(len(node.ReadyWhen))) //nolint:errcheck

		for _, c := range node.PropagateWhen {
			hashField([]byte(c))
		}
		binary.Write(h, binary.LittleEndian, int64(len(node.PropagateWhen))) //nolint:errcheck
	}

	return fmt.Sprintf("%016x", h.Sum64())
}

// ---------------------------------------------------------------------------
// CEL extension functions
// ---------------------------------------------------------------------------

// celPluralFunction returns CEL env options for the plural() function.
func celPluralFunction() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Function("plural",
			cel.Overload("plural_string",
				[]*cel.Type{cel.StringType},
				cel.StringType,
				cel.UnaryBinding(func(val ref.Val) ref.Val {
					s := val.Value().(string)
					return types.String(flect.Pluralize(s))
				}),
			),
		),
	}
}

// celReadyFunction returns CEL env options for the .ready() member function.
//
// .ready() returns whether the graph controller considers a node ready.
// The readiness state is injected into the scope data as "__ready" after
// each node is processed during the DAG walk:
//   - No readyWhen: __ready = true (applied = ready)
//   - With readyWhen: __ready = (all conditions passed)
//
// For scalar nodes (Watch, Own, Contribute), .ready() reads __ready from
// the object map. For collection nodes (forEach, WatchKind), .ready()
// returns true when ALL items have __ready == true — the collection's
// readiness is a function of its children's readiness.
//
// This enables expressions like:
//
//	propagateWhen: ["${dependency.ready()}"]
//	readyWhen: ["${workers.ready()}"]  // true when all forEach items ready
func celReadyFunction() []cel.EnvOption {
	impl := func(val ref.Val) ref.Val {
		native, err := conversion.GoNativeType(val)
		if err != nil {
			return types.Bool(false)
		}
		switch obj := native.(type) {
		case map[string]any:
			// Scalar node — read __ready directly
			ready, _ := obj["__ready"].(bool)
			return types.Bool(ready)
		case []any:
			// Collection node — all items must be ready
			if len(obj) == 0 {
				return types.Bool(true) // empty collection is vacuously ready
			}
			for _, item := range obj {
				m, ok := item.(map[string]any)
				if !ok {
					return types.Bool(false)
				}
				ready, _ := m["__ready"].(bool)
				if !ready {
					return types.Bool(false)
				}
			}
			return types.Bool(true)
		default:
			return types.Bool(false)
		}
	}
	return []cel.EnvOption{
		cel.Function("ready",
			cel.MemberOverload("dyn_ready",
				[]*cel.Type{cel.DynType},
				cel.BoolType,
				cel.UnaryBinding(impl),
			),
		),
	}
}

// celSimpleSchemaFunction returns CEL env options for simpleSchema.toOpenAPI().
// Converts a SimpleSchema definition to an OpenAPI v3 schema for use in CRD specs.
// The first argument is a schema map with spec/status/types fields.
// The second argument is a resources list (used for context, currently unused).
func celSimpleSchemaFunction() []cel.EnvOption {
	impl := func(schemaVal, resourcesVal ref.Val) ref.Val {
		reg := types.NewEmptyRegistry()

		schemaNative, err := conversion.GoNativeType(schemaVal)
		if err != nil {
			return types.NewErr("simpleSchema.toOpenAPI: converting schema: %v", err)
		}

		schemaMap, ok := schemaNative.(map[string]any)
		if !ok {
			return types.NewErr("simpleSchema.toOpenAPI: schema must be a map, got %T", schemaNative)
		}

		specMap, _ := schemaMap["spec"].(map[string]any)
		if specMap == nil {
			specMap = schemaMap
		}
		customTypes, _ := schemaMap["types"].(map[string]any)

		openAPISchema, err := simpleschema.ToOpenAPISpec(specMap, customTypes)
		if err != nil {
			return types.NewErr("simpleSchema.toOpenAPI: %v", err)
		}

		// JSON round-trip: JSONSchemaProps → map[string]any
		jsonBytes, err := json.Marshal(openAPISchema)
		if err != nil {
			return types.NewErr("simpleSchema.toOpenAPI: marshaling: %v", err)
		}
		var result map[string]any
		if err := json.Unmarshal(jsonBytes, &result); err != nil {
			return types.NewErr("simpleSchema.toOpenAPI: unmarshaling: %v", err)
		}

		// Wrap with standard Kubernetes object structure
		fullSchema := map[string]any{
			"type": "object",
			"properties": map[string]any{
				"apiVersion": map[string]any{"type": "string"},
				"kind":       map[string]any{"type": "string"},
				"metadata":   map[string]any{"type": "object"},
				"spec":       result,
				"status":     map[string]any{"type": "object"},
			},
		}

		// Convert status types if present (skip runtime ${} expressions)
		if statusMap, ok := schemaMap["status"].(map[string]any); ok && len(statusMap) > 0 {
			hasExpressions := false
			for _, v := range statusMap {
				if s, ok := v.(string); ok && strings.Contains(s, "${") {
					hasExpressions = true
					break
				}
			}
			if !hasExpressions {
				statusSchema, err := simpleschema.ToOpenAPISpec(statusMap, customTypes)
				if err != nil {
					return types.NewErr("simpleSchema.toOpenAPI: status: %v", err)
				}
				statusJSON, err := json.Marshal(statusSchema)
				if err != nil {
					return types.NewErr("simpleSchema.toOpenAPI: marshaling status: %v", err)
				}
				var statusResult map[string]any
				if err := json.Unmarshal(statusJSON, &statusResult); err != nil {
					return types.NewErr("simpleSchema.toOpenAPI: unmarshaling status: %v", err)
				}
				fullSchema["properties"].(map[string]any)["status"] = statusResult
			}
		}

		return reg.NativeToValue(fullSchema)
	}
	return []cel.EnvOption{
		cel.Function("simpleSchema.toOpenAPI",
			cel.Overload("simpleSchema_toOpenAPI",
				[]*cel.Type{cel.DynType, cel.DynType},
				cel.DynType,
				cel.BinaryBinding(impl),
			),
		),
	}
}
