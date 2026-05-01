// cel.go contains CEL runtime integration: expression compilation/evaluation
// and error classification for distinguishing retryable "data pending" errors
// from expression bugs.
//
// Custom CEL extension functions (plural, .ready(), simpleSchema.toOpenAPI(),
// .updated(), .dependencies()) are defined in celfuncs.go.
//
// Performance model: CEL environments and programs are compiled eagerly when a
// Graph spec is first seen (or when it changes). The reconcile loop only evaluates
// pre-compiled programs — no compilation happens during the resource walk.
//
// Per-instance mutable state (scope, input hashes, forEach state) is tracked
// separately in instanceState, keyed by namespace/revision-name.
package compiler

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
	celast "github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/types"

	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
	"github.com/ellistarn/kro/experimental/controller/graph"
	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/conversion"
	celunstructured "github.com/kubernetes-sigs/kro/pkg/cel/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	celopenapi "k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/kube-openapi/pkg/validation/spec"
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

// ErrDependencyError is an alias for dag.ErrCircularDependency.
// Kept for backward compatibility with status.go's error classification.
var ErrDependencyError = dagpkg.ErrCircularDependency

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

// IsPending checks if an error indicates data is pending — either a CEL
// runtime error (string pattern match) or a wrapped ErrPending sentinel.
func IsPending(err error) bool {
	return isCELPending(err) || errors.Is(err, ErrPending)
}

// ---------------------------------------------------------------------------
// Compiled graph (immutable)
// ---------------------------------------------------------------------------

// CompiledGraph holds the immutable compilation artifacts for a Graph spec.
// All fields are derived from the spec. The DAG topology, CEL programs, and
// CEL environment are all immutable after construction — cel.Program is
// thread-safe by the CEL spec, and BuildDAG produces a read-only topology.
// Per-instance concrete node bodies are assembled separately via assembleDAG.
type CompiledGraph struct {
	env            *cel.Env                          // CEL environment (immutable after Extend)
	Programs       map[string]cel.Program            // expression string → compiled program
	ExprPaths      map[string]map[string][]graph.FieldPath // expression string → (scope var → field paths)
	ExprAccessModes map[string]map[string]bool       // expression string → (scope var → optional-only access)
	declaredVars   map[string]bool                   // variable names declared in the CEL env
	Topology       *dagpkg.Topology                      // shared DAG structure (immutable after BuildDAG)
	UnresolvedGVKs []schema.GroupVersionKind         // GVKs that fell back to dyn (triggers recompilation on CRD install)
	// typeCacheGen records the type cache generation at compile time.
	// Per 004-compilation.md § Type Cache: "Staleness is one integer comparison:
	// current generation exceeds the artifact's recorded generation."
	TypeCacheGen int64
	// dynamicGVKNodes lists node IDs whose apiVersion or kind contains a CEL
	// expression. Per 004-compilation.md § Deferred Types: "the resolved GVK is
	// also recorded per-node in the artifact." These nodes are compiled
	// permissively; the reconciler detects GVK changes at runtime.
	DynamicGVKNodes []string
	// collectionIDs captures the set of Watch node IDs in this spec.
	// Used by the dynamic-compile fallback to apply the same
	// `<wk_id>.ready()` AST rewrite that the eager-compile path does —
	// expressions that reach the dynamic path (cross-revision
	// finalization, forEach-finalizer synthesis, ad-hoc test evals)
	// must honor the same rewrite, otherwise empty-Watch `.ready()`
	// reverts to vacuously-true on that path.
	CollectionIDs map[string]bool

	// ChildTopologies maps forEach node ID → pre-compiled child topological
	// order. Nil when no expression-valued child Graphs exist.
	ChildTopologies map[string][]string

	// resourceSchemas maps node ID → resolved OpenAPI schema. Used at Eval
	// time to wrap scope entries via UnstructuredToVal so schema-typed
	// fields (e.g. Secret data values declared format:"byte") arrive in
	// CEL as their declared runtime types (types.Bytes, not types.String).
	// Mirrors upstream pkg/runtime/node_context.go buildContext.
	ResourceSchemas map[string]*spec.Schema
}

// Env returns the CEL environment for use in tests and downstream compilation.
func (c *CompiledGraph) Env() *cel.Env {
	return c.env
}

// eval evaluates a CEL expression against the given scope.
// First checks the pre-compiled program cache; if the expression is not found
// (e.g., a readyWhen expression from a superseded revision evaluated using the
// current revision's evaluator), it compiles the expression on-the-fly using
// the current CEL environment. This handles cross-revision finalization where
// the snapshot's readyWhen may reference nodes declared in the current spec but
// whose expression was only pre-compiled in the old spec.
func (c *CompiledGraph) Eval(expr string, scope map[string]any) (any, error) {
	prg, ok := c.Programs[expr]
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
		if len(c.CollectionIDs) > 0 {
			nativeAst := parsed.NativeRep()
			nextIDVal := celast.MaxID(nativeAst)
			nextID := func() int64 {
				id := nextIDVal
				nextIDVal++
				return id
			}
			factory := celast.NewExprFactory()
			rewriteCollectionReady(nativeAst.Expr(), c.CollectionIDs, factory, nextID)
			// Build scope vars for .dependencies() rewrite in dynamic path.
			dynScopeVars := make(map[string]bool, len(c.declaredVars))
			for k := range c.declaredVars {
				dynScopeVars[k] = true
			}
			rewriteDependencies(nativeAst.Expr(), dynScopeVars, factory, nextID)
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

	out, _, err := prg.Eval(c.WrapScope(scope))
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

// wrapScope returns an activation map where scope entries for nodes with
// resolved OpenAPI schemas are wrapped via UnstructuredToVal. Without this,
// a Secret's data.key (declared format:"byte" in the OpenAPI spec) enters
// CEL as a raw base64 string, so string(secret.data.key) is an identity op
// rather than a decode. After wrapping, the runtime value conveys its
// declared type — schema-aware type conversion happens at field-access time.
//
// Wrapping is shallow and per-call: the original scope map is never mutated,
// so hash inputs, previousScope retention, and serialization paths still see
// plain map[string]any values.
//
// Entries without a schema (definitions, unresolved CRDs, forEach iterators)
// pass through unchanged — CEL's default type adapter handles them as before.
//
// Mirrors upstream's pkg/runtime/node_context.go buildContext behavior.
func (c *CompiledGraph) WrapScope(scope map[string]any) map[string]any {
	if len(c.ResourceSchemas) == 0 {
		return scope
	}
	wrapped := make(map[string]any, len(scope))
	for k, v := range scope {
		// Optional values (lazy deps) are already CEL-ready — pass through
		// without schema wrapping. The inner value was wrapped (if applicable)
		// when the dependency's scope entry was first published.
		if _, isOpt := v.(*types.Optional); isOpt {
			wrapped[k] = v
			continue
		}
		s, ok := c.ResourceSchemas[k]
		if !ok || s == nil {
			wrapped[k] = v
			continue
		}
		switch tv := v.(type) {
		case map[string]any:
			wrapped[k] = celunstructured.UnstructuredToVal(tv, &celopenapi.Schema{Schema: s})
		case []any:
			// Collection nodes: wrap each element with the item schema.
			items := make([]any, len(tv))
			for i, item := range tv {
				if m, ok := item.(map[string]any); ok {
					items[i] = celunstructured.UnstructuredToVal(m, &celopenapi.Schema{Schema: s})
				} else {
					items[i] = item
				}
			}
			wrapped[k] = items
		default:
			wrapped[k] = v
		}
	}
	return wrapped
}

// ---------------------------------------------------------------------------
// Compilation
// ---------------------------------------------------------------------------

// CompileGraphSpec builds a typed CEL environment, eagerly compiles every
// expression, and builds the dependency graph. Returns a CompiledGraph ready
// for sharing across multiple instances.
//
// The typeInfo parameter carries resolved types from ResolveNodeTypes. When nil,
// all nodes fall back to dyn.
func CompileGraphSpec(spec *graph.GraphSpec, typeInfo *TypeSource) (*CompiledGraph, error) {
	if typeInfo == nil {
		// No type information — all nodes declared as dyn (backwards compat).
		typeInfo = ResolveNodeTypes(spec.Nodes, nil)
	}

	// Phase 3: build the typed CEL environment.
	// All typed declarations (resource schemas + definition types) go through
	// a single DeclTypeProvider to avoid the double-provider problem.
	typedDecls := buildTypedEnvOptions(typeInfo)

	env, err := krocel.DefaultEnvironment(
		krocel.WithResourceIDs(typeInfo.UntypedIDs),
		krocel.WithListVariables(typeInfo.listIDs),
		krocel.WithCustomDeclarations(typedDecls),
		krocel.WithCustomDeclarations(customCELFunctions()),
		// __kroNodeReady carries per-Watch readyWhen verdicts, looked up
		// by the AST rewrite of `<wk_id>.ready()` (see readyrewrite.go).
		// Per 001-graph.md § readyWhen: "A Watch's `.ready()` returns
		// true when the node's readyWhen conditions pass."
		krocel.WithCustomDeclarations([]cel.EnvOption{
			cel.Variable(ReservedNodeReadyVar, cel.MapType(cel.StringType, cel.BoolType)),
			cel.Variable(ReservedDepsMapVar, cel.MapType(cel.StringType, cel.ListType(cel.DynType))),
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("creating CEL env: %w", err)
	}

	// Phase 3b: preliminary dependency scan — detect cycles before expensive
	// CEL compilation. Scans expression text for identifier references using
	// extractFirstIdentifier (no CEL parsing). Builds a dependency graph and
	// rejects cycles early.
	allIDs := spec.AllIdentifiers()
	if err := dagpkg.DetectCyclesEarly(spec.Nodes, allIDs); err != nil {
		return nil, err
	}

	// Phase 4: compile expressions and build DAG.

	// Build scope var set for field path extraction.
	scopeVars := make(map[string]bool, len(allIDs))
	for _, id := range allIDs {
		scopeVars[id] = true
	}

	expressions := spec.AllExpressions()

	// Watch node IDs: AST rewrite targets. Only these IDs have their
	// `.ready()` calls redirected to the readiness sidecar map.
	collectionIDs := make(map[string]bool, len(typeInfo.listIDs))
	for _, id := range typeInfo.listIDs {
		collectionIDs[id] = true
	}
	rewriteFactory := celast.NewExprFactory()

	programs := make(map[string]cel.Program, len(expressions))
	exprPaths := make(map[string]map[string][]graph.FieldPath, len(expressions))
	exprAccessModes := make(map[string]map[string]bool, len(expressions))
	exprTypes := make(map[string]*cel.Type, len(expressions))

	for _, expr := range expressions {
		// Parse first so we can rewrite `<wk_id>.ready()` into a scope
		// lookup BEFORE type checking. Parsing alone doesn't type-check,
		// so the pre-rewrite expression is accepted even though the
		// post-rewrite form is what actually compiles.
		parsed, issues := env.Parse(expr)
		if issues != nil && issues.Err() != nil {
			return nil, fmt.Errorf("parsing expression %q: %w: %w", expr, ErrInvalidExpression, issues.Err())
		}

		// Classify access modes on the pre-rewrite AST. The rewrite
		// replaces Watch .ready() with map lookups, losing the scope var
		// reference. Pre-rewrite classification captures the original
		// .ready()/.updated() patterns that signal optional access.
		exprAccessModes[expr] = classifyAccessModes(parsed.NativeRep().Expr(), scopeVars, nil)

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
		rewriteDependencies(nativeAst.Expr(), scopeVars, rewriteFactory, nextID)
		ast, issues := env.Check(parsed)
		if issues != nil && issues.Err() != nil {
			return nil, fmt.Errorf("checking expression %q: %w: %w", expr, ErrInvalidExpression, issues.Err())
		}
		exprTypes[expr] = ast.OutputType()
		// Extract field paths from the AST before creating the program.
		// Per 004-compilation.md § Algorithm, Phase 4 step 5: field path
		// extraction walks the AST at compile time. One walk per expression.
		exprPaths[expr] = extractFieldPathsFromAST(ast.NativeRep().Expr(), scopeVars, nil)
		prg, err := env.Program(ast)
		if err != nil {
			return nil, fmt.Errorf("programming expression %q: %w: %w", expr, ErrInvalidExpression, err)
		}
		programs[expr] = prg
	}

	// Phase 4a: type refinement (second pass).
	// Narrow def expression fields from dyn to their compiled return types.
	// Narrow forEach iterators from the collection expression's element type.
	// Re-check expressions that reference narrowed types against a new environment.
	refinedTypeInfo := refineDefTypes(spec.Nodes, typeInfo, exprTypes)
	if refinedTypeInfo != nil {
		// Types narrowed — rebuild the environment and re-check affected expressions.
		refinedDecls := buildTypedEnvOptions(refinedTypeInfo)
		refinedEnv, err := krocel.DefaultEnvironment(
			krocel.WithResourceIDs(refinedTypeInfo.UntypedIDs),
			krocel.WithListVariables(refinedTypeInfo.listIDs),
			krocel.WithCustomDeclarations(refinedDecls),
			krocel.WithCustomDeclarations(customCELFunctions()),
			krocel.WithCustomDeclarations([]cel.EnvOption{
				cel.Variable(ReservedNodeReadyVar, cel.MapType(cel.StringType, cel.BoolType)),
				cel.Variable(ReservedDepsMapVar, cel.MapType(cel.StringType, cel.ListType(cel.DynType))),
			}),
		)
		if err != nil {
			return nil, fmt.Errorf("creating refined CEL env: %w", err)
		}
		// Re-check all expressions against the refined environment.
		// This catches type errors that were invisible against dyn.
		for _, expr := range expressions {
			parsed, issues := refinedEnv.Parse(expr)
			if issues != nil && issues.Err() != nil {
				// Parse errors were caught in the first pass — should not happen.
				continue
			}
			nativeAst := parsed.NativeRep()
			nextIDVal := celast.MaxID(nativeAst)
			nextID := func() int64 {
				id := nextIDVal
				nextIDVal++
				return id
			}
			rewriteCollectionReady(nativeAst.Expr(), collectionIDs, rewriteFactory, nextID)
			rewriteDependencies(nativeAst.Expr(), scopeVars, rewriteFactory, nextID)
			_, issues = refinedEnv.Check(parsed)
			if issues != nil && issues.Err() != nil {
				return nil, fmt.Errorf("checking expression %q (type refinement): %w: %w", expr, ErrInvalidExpression, issues.Err())
			}
		}
		// Use the refined environment for the compiled graph.
		env = refinedEnv
	}

	// Phase 4a-2: validate expression-to-field compatibility.
	// Check that standalone expression return types match the destination
	// field's schema type. Only for nodes with resolved schemas.
	if err := validateExprFieldCompat(spec.Nodes, typeInfo, exprTypes); err != nil {
		return nil, err
	}

	// Phase 4a-3: forEach readyWhen inner-scope compilation.
	// Per 004-compilation.md § Algorithm: "Inner scope (readyWhen) binds the
	// iterator to the collection's element type. Outer scope (downstream)
	// binds the node ID to the list type."
	// For each forEach node with readyWhen AND a resolved schema, build an
	// inner env where the node ID is the element type (not list type) and
	// re-check readyWhen expressions against it.
	for _, node := range spec.Nodes {
		if node.ForEach == nil || len(node.ReadyWhen) == 0 {
			continue
		}
		s, ok := typeInfo.forEachSchemas[node.ID]
		if !ok || s == nil {
			continue // unresolved schema — readyWhen compiles against dyn (permissive)
		}
		// Build element-typed declaration for the node ID.
		declType := krocel.SchemaDeclTypeWithMetadata(&celopenapi.Schema{Schema: s}, false)
		if declType == nil {
			continue
		}
		typeName := krocel.TypeNamePrefix + node.ID
		declType = declType.MaybeAssignTypeName(typeName)
		// Build inner env: create a fresh env with the element type for this
		// node ID. We can't use env.Extend because the node ID may already be
		// declared in the outer env (as dyn). Build a minimal env instead.
		innerProvider := krocel.NewDeclTypeProvider(declType)
		innerProvider.SetRecognizeKeywordAsFieldName(true)
		innerRegistry := types.NewEmptyRegistry()
		wrappedInner, err := innerProvider.WithTypeProvider(innerRegistry)
		if err != nil {
			continue
		}
		// Filter out the forEach node ID from untypedIDs to avoid overlap.
		var filteredIDs []string
		for _, id := range typeInfo.UntypedIDs {
			if id != node.ID {
				filteredIDs = append(filteredIDs, id)
			}
		}
		innerEnv, err := krocel.DefaultEnvironment(
			krocel.WithResourceIDs(filteredIDs),
			krocel.WithListVariables(typeInfo.listIDs),
			krocel.WithCustomDeclarations([]cel.EnvOption{
				cel.CustomTypeProvider(wrappedInner),
				cel.Variable(node.ID, declType.CelType()), // element type, not list
			}),
			krocel.WithCustomDeclarations(customCELFunctions()),
			krocel.WithCustomDeclarations([]cel.EnvOption{
				cel.Variable(ReservedNodeReadyVar, cel.MapType(cel.StringType, cel.BoolType)),
				cel.Variable(ReservedDepsMapVar, cel.MapType(cel.StringType, cel.ListType(cel.DynType))),
			}),
		)
		if err != nil {
			continue
		}
		// Re-check readyWhen expressions against the inner env.
		for _, rwExpr := range node.ReadyWhen {
			// Extract inner expression from ${...}
			dollars, expr, start, end := graph.FindExpr(rwExpr, 0)
			if start < 0 || len(dollars) != 1 || start != 0 || end != len(rwExpr) {
				continue
			}
			parsed, issues := innerEnv.Parse(expr)
			if issues != nil && issues.Err() != nil {
				continue // parse error caught by main compilation
			}
			nativeAst := parsed.NativeRep()
			nextIDVal := celast.MaxID(nativeAst)
			nextID := func() int64 {
				id := nextIDVal
				nextIDVal++
				return id
			}
			rewriteCollectionReady(nativeAst.Expr(), collectionIDs, rewriteFactory, nextID)
			rewriteDependencies(nativeAst.Expr(), scopeVars, rewriteFactory, nextID)
			_, issues = innerEnv.Check(parsed)
			if issues != nil && issues.Err() != nil {
				return nil, fmt.Errorf("node %q: readyWhen expression %q (forEach inner scope): %w: %w",
					node.ID, expr, ErrInvalidExpression, issues.Err())
			}
		}
	}

	// Phase 4b: validate forEach collection expressions return a list.
	// A forEach expression must be a standalone ${expr} whose output type
	// is list(T). Deferred expressions ($${...}) are validated by the
	// child graph's compiler. Expressions typed as dyn are permissive —
	// we can't statically prove they're wrong.
	for _, node := range spec.Nodes {
		if node.ForEach == nil {
			continue
		}
		dollars, innerExpr, start, end := graph.FindExpr(node.ForEach.Expr, 0)
		if start < 0 {
			// No ${...} expression at all — a literal string is never iterable.
			return nil, fmt.Errorf("node %q: forEach expression %q must contain a ${...} expression: %w",
				node.ID, node.ForEach.Expr, ErrInvalidExpression)
		}
		if len(dollars) != 1 {
			continue // deferred ($${...}) — validated by child compiler
		}
		// The entire string must be ${expr}, not an interpolation like
		// "prefix-${expr}-suffix". Interpolation always produces a string,
		// which is never iterable.
		if start != 0 || end != len(node.ForEach.Expr) {
			return nil, fmt.Errorf("node %q: forEach expression must be a standalone ${...}, got %q: %w",
				node.ID, node.ForEach.Expr, ErrInvalidExpression)
		}
		outputType, ok := exprTypes[innerExpr]
		if !ok {
			continue // expression not in compilation set (shouldn't happen)
		}
		// dyn/any are permissive — can't statically prove they're not a list.
		// AnyType (google.protobuf.Any) is how unresolved resource IDs are
		// declared; DynType is the CEL-native dynamic type. Both mean
		// "type unknown at compile time."
		if outputType == cel.DynType || outputType == cel.AnyType {
			continue
		}
		if _, err := krocel.ListElementType(outputType); err != nil {
			return nil, fmt.Errorf("node %q: forEach expression %q must return a list, got %q: %w",
				node.ID, innerExpr, outputType, ErrInvalidExpression)
		}
	}

	// Phase 4c: validate deferred ($${...}) expressions at each deferral depth.
	// Per 004-compilation.md § Deferred Expression Analysis.
	if err := compileDeferredExpressions(spec); err != nil {
		return nil, err
	}

	// Build the dependency graph using pre-extracted field paths.
	// Cycle detection happens here — a cycle in the dependency graph
	// sets Compiled=False with DependencyError reason.
	dag, err := dagpkg.BuildDAG(spec.Nodes, exprPaths, exprAccessModes)
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
	declared[ReservedNodeReadyVar] = true
	declared[ReservedDepsMapVar] = true

	var dynamicGVKNodes []string
	if typeInfo != nil {
		dynamicGVKNodes = typeInfo.DynamicGVKNodes
	}
	return &CompiledGraph{
		env:             env,
		Programs:        programs,
		ExprPaths:       exprPaths,
		ExprAccessModes: exprAccessModes,
		declaredVars:    declared,
		Topology:        dag.Topology,
		UnresolvedGVKs:  typeInfo.UnresolvedGVKs,
		DynamicGVKNodes: dynamicGVKNodes,
		CollectionIDs:   collectionIDs,
		ResourceSchemas: typeInfo.ResourceSchemas,
	}, nil
}


