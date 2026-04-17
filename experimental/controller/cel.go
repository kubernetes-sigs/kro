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
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
	celast "github.com/google/cel-go/common/ast"
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
	// collectionIDs captures the set of Watch node IDs in this spec.
	// Used by the dynamic-compile fallback to apply the same
	// `<wk_id>.ready()` AST rewrite that the eager-compile path does —
	// expressions that reach the dynamic path (cross-revision
	// finalization, forEach-finalizer synthesis, ad-hoc test evals)
	// must honor the same rewrite, otherwise empty-Watch `.ready()`
	// reverts to vacuously-true on that path.
	collectionIDs map[string]bool
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
		// Parse + AST-rewrite `<wk_id>.ready()` before type-check so the
		// dynamic path matches the eager-compile path. Without this,
		// expressions compiled here (cross-revision finalization,
		// forEach-finalizer synthesized expressions, and test-harness
		// eval calls that reference unregistered expressions) would
		// keep the original `.ready()` behavior — vacuously-true on
		// empty Watch collections.
		parsed, issues := compileEnv.Parse(expr)
		if issues != nil && issues.Err() != nil {
			return nil, fmt.Errorf("expression %q not in cache, dynamic parse failed: %w", expr, issues.Err())
		}
		if len(c.collectionIDs) > 0 {
			nativeAst := parsed.NativeRep()
			nextIDVal := celast.MaxID(nativeAst)
			nextID := func() int64 {
				id := nextIDVal
				nextIDVal++
				return id
			}
			rewriteCollectionReady(nativeAst.Expr(), c.collectionIDs, celast.NewExprFactory(), nextID)
		}
		ast, issues := compileEnv.Check(parsed)
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
		// __kroNodeReady carries per-Watch readyWhen verdicts, looked up
		// by the AST rewrite of `<wk_id>.ready()` (see readyrewrite.go).
		// Per 001-graph.md § readyWhen: "A Watch's `.ready()` returns
		// true when the node's readyWhen conditions pass."
		krocel.WithCustomDeclarations([]cel.EnvOption{
			cel.Variable(reservedNodeReadyVar, cel.MapType(cel.StringType, cel.BoolType)),
		}),
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

	// Watch node IDs: AST rewrite targets. Only these IDs have their
	// `.ready()` calls redirected to the readiness sidecar map.
	collectionIDs := make(map[string]bool, len(typeInfo.listIDs))
	for _, id := range typeInfo.listIDs {
		collectionIDs[id] = true
	}
	rewriteFactory := celast.NewExprFactory()

	expressions := spec.AllExpressions()
	programs := make(map[string]cel.Program, len(expressions))
	exprPaths := make(map[string]map[string][]FieldPath, len(expressions))

	for _, expr := range expressions {
		// Parse first so we can rewrite `<wk_id>.ready()` into a scope
		// lookup BEFORE type checking. Parsing alone doesn't type-check,
		// so the pre-rewrite expression is accepted even though the
		// post-rewrite form is what actually compiles.
		parsed, issues := env.Parse(expr)
		if issues != nil && issues.Err() != nil {
			return nil, fmt.Errorf("parsing expression %q: %w: %w", expr, ErrInvalidExpression, issues.Err())
		}
		// Fresh IDs for any rewritten sub-expressions. CEL IDs are
		// per-AST, monotonic. Seeding at MaxID avoids collisions with
		// existing IDs; collisions produce "incompatible type already
		// exists" errors at type-check.
		nativeAst := parsed.NativeRep()
		nextIDVal := celast.MaxID(nativeAst)
		nextID := func() int64 {
			id := nextIDVal
			nextIDVal++
			return id
		}
		rewriteCollectionReady(nativeAst.Expr(), collectionIDs, rewriteFactory, nextID)
		ast, issues := env.Check(parsed)
		if issues != nil && issues.Err() != nil {
			return nil, fmt.Errorf("checking expression %q: %w: %w", expr, ErrInvalidExpression, issues.Err())
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
	declared := make(map[string]bool, len(allIDs)+1)
	for _, id := range allIDs {
		declared[id] = true
	}
	// The readiness sidecar is a reserved scope variable declared in the
	// env above. Must be listed as declared so the dynamic-compile
	// fallback does not re-declare it (which produces an "overlapping
	// identifier" error at extend time).
	declared[reservedNodeReadyVar] = true

	return &compiledGraph{
		specHash:       spec.Hash(),
		env:            env,
		programs:       programs,
		exprPaths:      exprPaths,
		declaredVars:   declared,
		spec:           spec,
		dag:            dag,
		unresolvedGVKs: typeInfo.unresolvedGVKs,
		collectionIDs:  collectionIDs,
	}, nil
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
// the object map. For forEach parents, .ready() returns true when ALL items
// have __ready == true — the collection's readiness is a function of its
// children's readiness.
//
// For Watch nodes, `.ready()` is rewritten at compile time to a
// scope-variable lookup (see readyrewrite.go). That redirection surfaces
// the node's readyWhen verdict directly — independent of the collection
// being non-empty — so `pods.ready()` returns the correct value even when
// the collection has zero items. This function's list branch is kept for
// forEach parents whose readiness is aggregated from children's per-item
// __ready stamping. Per 001-graph.md § readyWhen.
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
				"status": map[string]any{
					"type":                                 "object",
					"x-kubernetes-preserve-unknown-fields": true,
				},
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
