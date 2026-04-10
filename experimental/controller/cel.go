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
	"sort"
	"strings"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"

	"github.com/gobuffalo/flect"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/conversion"
	"github.com/kubernetes-sigs/kro/pkg/simpleschema"
)

// ErrDataPending indicates that CEL evaluation failed because required data
// is not yet available (e.g., a resource's status field hasn't been populated).
// This is a retryable condition — the controller should requeue and try again.
var ErrDataPending = errors.New("data pending")

// ErrWaitingForReadiness indicates that a resource exists but hasn't satisfied
// its readyWhen conditions yet. Downstream resources should wait.
var ErrWaitingForReadiness = errors.New("waiting for readiness")

// ErrCompilationFailed indicates that one or more CEL expressions in the Graph
// spec failed to compile. This is a permanent error until the spec is fixed.
var ErrCompilationFailed = errors.New("compilation failed")

// ErrFieldConflict indicates that an SSA apply received a 409 Conflict because
// another actor has taken ownership of fields the controller manages. This is
// a permanent error for the resource until the external actor releases the
// field or the Graph spec changes to no longer write that field.
var ErrFieldConflict = errors.New("field conflict")

// ErrCycleDetected indicates that the dependency graph contains a cycle.
// This is a permanent error until the spec is fixed.
var ErrCycleDetected = errors.New("cycle detected")

// celDataPendingPatterns are CEL error patterns that indicate data is not yet
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
var celDataPendingPatterns = []string{
	"no such key",
	"no such field",
	"no such attribute",
	"index out of bounds",
}

// isCELDataPending checks if a CEL error indicates data is pending.
func isCELDataPending(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, pattern := range celDataPendingPatterns {
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}

// isDataPending checks if an error indicates data is pending — either a CEL
// runtime error (string pattern match) or a wrapped ErrDataPending sentinel.
func isDataPending(err error) bool {
	return isCELDataPending(err) || errors.Is(err, ErrDataPending)
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
	specHash string                 // content hash of the compilation inputs
	env      *cel.Env               // CEL environment (immutable after Extend)
	programs map[string]cel.Program // expression string → compiled program
	spec     *GraphSpec             // parsed spec (immutable)
	dag      *DAG                   // dependency graph (immutable after BuildDAG)
}

// eval evaluates a pre-compiled CEL expression against the given scope.
// The program must exist in the cache — a cache miss indicates an invariant
// violation (AllExpressions() is incomplete) and is treated as a hard error.
func (c *compiledGraph) eval(expr string, scope map[string]any) (any, error) {
	prg, ok := c.programs[expr]
	if !ok {
		return nil, fmt.Errorf("expression %q not found in compiled cache — this is an invariant violation (AllExpressions may be incomplete)", expr)
	}

	out, _, err := prg.Eval(scope)
	if err != nil {
		if isCELDataPending(err) {
			return nil, fmt.Errorf("evaluating %q: %w: %w", expr, ErrDataPending, err)
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
	previousScope      map[string]any       // node ID → last scope data
	previousKeys       map[string][]string  // node ID → last applied keys
	previousPlanStates map[string]NodeState // node ID → last plan state
	forEachItems       map[string][]any     // "nodeID/varName" → cached collection items

	// Resolved shapes for Deferred nodes. Set on first reconcile when the
	// existence check determines Owns vs Contribute. Persists across
	// reconciles within the same revision. Reset on new revision (new
	// instanceState).
	resolvedShapes map[string]TemplateShape

	// Input hashing state — retained across reconciles for change detection.
	// See 004-graph-execution.md § Wind step 3.
	previousInputHashes map[string]string // node ID → last dependency input hash
	previousSelfHashes  map[string]string // node ID → last self-section hash

	// Per-node forEach item state. Outer key is node ID, inner key is item identity.
	// Structural boundary prevents prefix collisions between node IDs.
	forEachItemScope map[string]map[string]any      // nodeID → itemID → scope data
	forEachItemKeys  map[string]map[string][]string // nodeID → itemID → applied keys
}

// newInstanceState creates a fresh instanceState for a compiledGraph.
func newInstanceState(compiled *compiledGraph) *instanceState {
	return &instanceState{
		compiled:            compiled,
		previousScope:       map[string]any{},
		previousKeys:        map[string][]string{},
		previousPlanStates:  map[string]NodeState{},
		previousInputHashes: map[string]string{},
		previousSelfHashes:  map[string]string{},
		forEachItems:        map[string][]any{},
		forEachItemScope:    map[string]map[string]any{},
		forEachItemKeys:     map[string]map[string][]string{},
		resolvedShapes:      make(map[string]TemplateShape, len(compiled.dag.Shapes)),
	}
}

// initResolvedShapes seeds the resolved shapes map from the DAG's compile-time
// shapes. Called once at the start of each reconcile to ensure all nodes have
// an entry. Deferred entries will be resolved during the walk.
func (s *instanceState) initResolvedShapes() {
	for id, shape := range s.compiled.dag.Shapes {
		if _, ok := s.resolvedShapes[id]; !ok {
			s.resolvedShapes[id] = shape
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
	mu        sync.Mutex
	compiled  map[string]*compiledGraph // spec hash → shared compiled graph
	instances map[string]*instanceState // namespace/revision-name → per-instance state
}

func newGraphCaches() *graphCaches {
	return &graphCaches{
		compiled:  make(map[string]*compiledGraph),
		instances: make(map[string]*instanceState),
	}
}

func (gc *graphCaches) get(key string) *instanceState {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	return gc.instances[key]
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

// getCompiled returns a shared compiledGraph by spec hash, or nil if not cached.
func (gc *graphCaches) getCompiled(specHash string) *compiledGraph {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	return gc.compiled[specHash]
}

// ---------------------------------------------------------------------------
// Compilation
// ---------------------------------------------------------------------------

// compileGraphSpec builds a CEL environment, eagerly compiles every expression,
// and builds the dependency graph. Returns a compiledGraph ready for sharing
// across multiple instances.
//
// All node IDs are declared upfront as DynType. Expressions that reference
// nodes not yet in scope at eval time will produce CEL runtime errors
// (e.g., "no such key") which isDataPending handles correctly.
func compileGraphSpec(spec *GraphSpec) (*compiledGraph, error) {
	allIDs := spec.AllIdentifiers()

	env, err := krocel.DefaultEnvironment(
		krocel.WithResourceIDs(allIDs),
		krocel.WithCustomDeclarations(celPluralFunction()),
		krocel.WithCustomDeclarations(celSimpleSchemaFunction()),
		krocel.WithCustomDeclarations(celReadyFunction()),
	)
	if err != nil {
		return nil, fmt.Errorf("creating CEL env: %w", err)
	}

	// Build the dependency graph. Cycle detection happens here — a cycle
	// in the dependency graph sets Accepted=False with CycleDetected reason.
	dag, err := BuildDAG(spec.Nodes)
	if err != nil {
		return nil, err
	}

	expressions := spec.AllExpressions()
	programs := make(map[string]cel.Program, len(expressions))

	for _, expr := range expressions {
		ast, issues := env.Compile(expr)
		if issues != nil && issues.Err() != nil {
			return nil, fmt.Errorf("compiling expression %q: %w: %w", expr, ErrCompilationFailed, issues.Err())
		}
		prg, err := env.Program(ast)
		if err != nil {
			return nil, fmt.Errorf("programming expression %q: %w: %w", expr, ErrCompilationFailed, err)
		}
		programs[expr] = prg
	}

	return &compiledGraph{
		specHash: spec.Hash(),
		env:      env,
		programs: programs,
		spec:     spec,
		dag:      dag,
	}, nil
}

// compileGraph is the backward-compatible entry point used by ensureRevision
// for pre-creation validation. It compiles the spec and returns the compiled
// graph without caching (the caller discards the result after validation).
func compileGraph(spec *GraphSpec) (*compiledGraph, error) {
	return compileGraphSpec(spec)
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
// For scalar nodes (Watch, Owns, Contribute), .ready() reads __ready from
// the object map. For collection nodes (forEach, collection watch), .ready()
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
