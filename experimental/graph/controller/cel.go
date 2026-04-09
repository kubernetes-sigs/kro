// cel.go contains CEL runtime integration: expression compilation/evaluation,
// custom CEL functions (plural, simpleSchema.toOpenAPI), and error classification
// for distinguishing retryable "data pending" errors from expression bugs.
//
// Performance model: CEL environments and programs are compiled eagerly when a
// Graph spec is first seen (or when it changes). The reconcile loop only evaluates
// pre-compiled programs — no compilation happens during the resource walk.
package graphcontroller

import (
	"encoding/json"
	"errors"
	"fmt"
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
// Expression cache
// ---------------------------------------------------------------------------

// graphCache holds the compiled CEL environment, programs, parsed spec, and
// DAG for a single Graph spec. All are derived from the spec and cached
// together, invalidated when the spec changes (detected by generation).
type graphCache struct {
	generation int64
	env        *cel.Env
	programs   map[string]cel.Program // expression string -> compiled program
	spec       *GraphSpec             // parsed spec (cached to avoid re-parsing)
	dag        *DAG                   // dependency graph (cached to avoid re-building)

	// State retained across reconciles for propagateWhen and forEach diffing.
	previousScope      map[string]any       // node ID → last scope data
	previousKeys       map[string][]string  // node ID → last applied keys
	previousPlanStates map[string]NodeState // node ID → last plan state
	forEachItems       map[string][]any     // "nodeID/varName" → cached collection items

	// Per-node forEach item state. Outer key is node ID, inner key is item identity.
	// Structural boundary prevents prefix collisions between node IDs.
	forEachItemScope map[string]map[string]any      // nodeID → itemID → scope data
	forEachItemKeys  map[string]map[string][]string // nodeID → itemID → applied keys
}

// graphCaches is a concurrent-safe map of Graph name → cache.
// Entries are created on first reconcile and removed on Graph deletion.
type graphCaches struct {
	mu     sync.Mutex
	caches map[string]*graphCache // keyed by namespace/name
}

func newGraphCaches() *graphCaches {
	return &graphCaches{caches: make(map[string]*graphCache)}
}

func (gc *graphCaches) get(key string) *graphCache {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	return gc.caches[key]
}

func (gc *graphCaches) set(key string, c *graphCache) {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	gc.caches[key] = c
}

func (gc *graphCaches) remove(key string) {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	delete(gc.caches, key)
}

// compileGraph builds a CEL environment with all node IDs declared and
// eagerly compiles every expression in the spec. Returns a graphCache ready
// for use during the reconcile loop, or an error if any expression fails to
// compile.
//
// All node IDs are declared upfront as DynType. Expressions that reference
// nodes not yet in scope at eval time will produce CEL runtime errors
// (e.g., "no such key") which isDataPending handles correctly.
func compileGraph(spec *GraphSpec, generation int64) (*graphCache, error) {
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

	return &graphCache{
		generation:         generation,
		env:                env,
		programs:           programs,
		spec:               spec,
		dag:                dag,
		previousScope:      map[string]any{},
		previousKeys:       map[string][]string{},
		previousPlanStates: map[string]NodeState{},
		forEachItems:       map[string][]any{},
		forEachItemScope:   map[string]map[string]any{},
		forEachItemKeys:    map[string]map[string][]string{},
	}, nil
}

// eval evaluates a pre-compiled CEL expression against the given scope.
// The program must exist in the cache — a cache miss indicates an invariant
// violation (AllExpressions() is incomplete) and is treated as a hard error.
func (c *graphCache) eval(expr string, scope map[string]any) (any, error) {
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
