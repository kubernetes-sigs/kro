// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package graph

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/google/cel-go/cel"
	celast "github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/cel/library"
	"github.com/kubernetes-sigs/kro/pkg/graph/dag"
)

// conditionTypePattern is copied from the +kubebuilder:validation:Pattern
// annotation on metav1.Condition.Type (k8s.io/apimachinery meta/v1/types.go).
const conditionTypePattern = `^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])$`

var conditionTypeRegex = regexp.MustCompile(conditionTypePattern)

// opaqueRegex matches a subexpression whose value is not statically known.
const opaqueRegex = `.*`

// maxConditionTypeValues caps how many values one `type:` expression may produce.
// Concatenation multiplies the operand counts, so chained ternaries would
// otherwise grow exponentially.
const maxConditionTypeValues = 32

var opaqueShape = []string{opaqueRegex}

// conditionPattern is the set of condition types one conditions entry could emit.
// A pattern rather than a name because the RGD compiles before any instance
// exists, so a computed `type:` is only known by its shape.
type conditionPattern struct {
	EntryIndex int
	matcher    *regexp.Regexp
}

func (p conditionPattern) matches(typ string) bool {
	return p.matcher != nil && p.matcher.MatchString(typ)
}

// typeRegexes splits the matcher back into one regex per condition type.
func (p conditionPattern) typeRegexes() []string {
	if p.matcher == nil {
		return nil
	}
	src := strings.TrimSuffix(strings.TrimPrefix(p.matcher.String(), "^(?:"), ")$")

	var out []string
	start := 0
	for i := 0; i < len(src); i++ {
		switch src[i] {
		case '\\':
			i++
		case '|':
			out = append(out, src[start:i])
			start = i + 1
		}
	}
	return append(out, src[start:])
}

// literalTypes returns the pattern's condition types, or ok=false if any has a
// computed segment. All-or-nothing over the whole set: in `cond ? "a" : (x + y)`
// the literal "a" is only reached when cond holds, so a sibling entry may own it.
func (p conditionPattern) literalTypes() (types []string, ok bool) {
	regexes := p.typeRegexes()
	types = make([]string, 0, len(regexes))
	for _, re := range regexes {
		if !isStaticType(re) {
			return nil, false
		}
		types = append(types, unquoteMeta(re))
	}
	return types, true
}

// String renders the pattern as an author-readable glob, with "*" for each
// computed segment ("a-*", "*-b").
func (p conditionPattern) String() string {
	regexes := p.typeRegexes()
	globs := make([]string, len(regexes))
	for i, re := range regexes {
		if isStaticType(re) {
			globs[i] = unquoteMeta(re)
			continue
		}
		globs[i] = unquoteMeta(strings.ReplaceAll(re, opaqueRegex, "*"))
	}
	return strings.Join(globs, "|")
}

// isStaticType reports whether a type regex is a plain literal.
func isStaticType(re string) bool { return !strings.Contains(re, opaqueRegex) }

// unquoteMeta drops the escaping backslashes from a regex, recovering the plain
// string it matches.
func unquoteMeta(re string) string {
	if !strings.ContainsRune(re, '\\') {
		return re
	}
	var b strings.Builder
	b.Grow(len(re))
	for i := 0; i < len(re); i++ {
		if re[i] == '\\' && i+1 < len(re) {
			i++
		}
		b.WriteByte(re[i])
	}
	return b.String()
}

// typeShape returns the regexes the `type:` expression could evaluate to, or nil
// when the alternation cap is exceeded.
func typeShape(e celast.Expr) []string {
	switch e.Kind() {
	case celast.LiteralKind:
		if s, ok := e.AsLiteral().Value().(string); ok {
			// QuoteMeta so "a.b" does not match a lookup of "aXb".
			return []string{regexp.QuoteMeta(s)}
		}
		return opaqueShape // non-string literal; the type checker rejects it

	case celast.CallKind:
		call := e.AsCall()
		if call.IsMemberFunction() {
			return opaqueShape // .join(), .format(), .replace()
		}
		switch call.FunctionName() {
		case operators.Add: // a + b -> cross product
			a, b := typeShape(call.Args()[0]), typeShape(call.Args()[1])
			if a == nil || b == nil || len(a)*len(b) > maxConditionTypeValues {
				return nil
			}
			out := make([]string, 0, len(a)*len(b))
			for _, x := range a {
				for _, y := range b {
					out = append(out, x+y)
				}
			}
			return out

		case operators.Conditional: // c ? t : f -> union of the branches
			t, f := typeShape(call.Args()[1]), typeShape(call.Args()[2])
			if t == nil || f == nil || len(t)+len(f) > maxConditionTypeValues {
				return nil
			}
			return append(t, f...)

		default:
			return opaqueShape // unknown global call, including indexing
		}

	default:
		return opaqueShape // field access (s.metadata.name), ident, list...
	}
}

// patternFromTypeExpr builds the declaration pattern for one newCondition's
// `type:` value.
func patternFromTypeExpr(entry int, typeExpr celast.Expr, exprText string) (conditionPattern, error) {
	frags := typeShape(typeExpr)
	if frags == nil {
		return conditionPattern{}, fmt.Errorf(
			"conditions[%d]: condition type expression has more than %d possible values; "+
				"split it across separate conditions entries, in expression %q",
			entry, maxConditionTypeValues, exprText)
	}

	slices.Sort(frags)
	frags = slices.Compact(frags)

	return conditionPattern{
		EntryIndex: entry,
		matcher:    regexp.MustCompile("^(?:" + strings.Join(frags, "|") + ")$"),
	}, nil
}

// conditionDeclarations returns one pattern per runtime.newCondition call under
// root: the types this entry could emit.
func conditionDeclarations(entry int, root celast.Expr, exprText string) ([]conditionPattern, error) {
	var out []conditionPattern
	var firstErr error
	celast.PreOrderVisit(root, celast.NewExprVisitor(func(e celast.Expr) {
		if firstErr != nil || !isRuntimeCall(e, "newCondition") {
			return
		}
		args := e.AsCall().Args()
		if len(args) != 1 {
			return // Wrong arity is a CEL type-checker concern.
		}
		typeExpr, ok := mapLiteralEntryValue(args[0], "type")
		if !ok {
			return // The parse-time macro requires a map literal with `type`.
		}
		p, err := patternFromTypeExpr(entry, typeExpr, exprText)
		if err != nil {
			firstErr = err
			return
		}
		out = append(out, p)
	}))
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// conditionDependencyTypes returns the literal type names entry root looks up via
// runtime.condition(schema, 'X'), sorted for stable diagnostics.
//
// Lookups on child resources are excluded: those come from observed state and are
// already sequenced by the resource DAG. Non-literal names are excluded because
// they are not known until evaluation, by which point the order is fixed.
func conditionDependencyTypes(root celast.Expr) []string {
	seen := map[string]struct{}{}
	celast.PreOrderVisit(root, celast.NewExprVisitor(func(e celast.Expr) {
		if !isRuntimeCall(e, "condition") {
			return
		}
		args := e.AsCall().Args()
		if len(args) != 2 {
			return
		}
		if obj := args[0]; obj.Kind() != celast.IdentKind || obj.AsIdent() != SchemaVarName {
			return
		}
		if typ, ok := literalString(args[1]); ok {
			seen[typ] = struct{}{}
		}
	}))
	return slices.Sorted(maps.Keys(seen))
}

// conditionEvalPlan is the build-time evaluation plan for one conditions block.
// Both slices are indexed by DECLARATION position. A rule violation is returned
// as an error instead, so holding a plan means the block is valid.
type conditionEvalPlan struct {
	// Ranks[i] is entry i's rank; entries are evaluated in ascending rank.
	Ranks []int

	// DependsOn[i] is what entry i must have available before it is evaluated.
	DependsOn [][]ConditionDependency
}

// validateAndOrderConditions applies build-time rules to the author's condition
// expressions and ranks each so an entry is evaluated after every entry that
// could declare a type it reads:
//
//   - runtime.condition(schema, 'X') with a literal X must name a kro built-in
//     type or a type another entry could declare; a self-reference or a
//     reference cycle is rejected, anything else is unknown. Child-resource and
//     non-literal lookups are unrestricted.
//   - Literal newCondition values: status must be True/False/Unknown and
//     type must not be empty. Computed values are checked at evaluation.
//   - No two entries may declare the same literal type; a type may repeat
//     within one entry (ternary branches).
//
// newCondition's key rules are enforced by its parse-time macro. Unparsable
// expressions are skipped; the regular compile step reports those errors.
func validateAndOrderConditions(env *cel.Env, expressions []string) (*conditionEvalPlan, error) {
	asts := make([]celast.Expr, len(expressions))
	for i, expr := range expressions {
		parsed, iss := env.Parse(expr)
		if iss.Err() != nil {
			continue
		}
		asts[i] = parsed.NativeRep().Expr()
	}

	var patterns []conditionPattern
	declares := make([][]conditionPattern, len(asts))
	dependencyTypes := make([][]string, len(asts))
	for i, root := range asts {
		if root == nil {
			continue
		}
		ps, err := conditionDeclarations(i, root, expressions[i])
		if err != nil {
			return nil, err
		}
		declares[i] = ps
		patterns = append(patterns, ps...)
		dependencyTypes[i] = conditionDependencyTypes(root)
	}

	// Reject invalid type names and types declared by more than one entry.
	exactBy := map[string]int{}
	for _, p := range patterns {
		// A computed pattern claims no single name; the runtime's collision check
		// covers it.
		names, ok := p.literalTypes()
		if !ok {
			continue
		}
		for _, name := range names {
			// Empty is left to validateConditionAST, which reports it plainly.
			if name == "" {
				continue
			}
			if !conditionTypeRegex.MatchString(name) {
				return nil, fmt.Errorf(
					"conditions[%d]: %q is not a valid condition type", p.EntryIndex, name)
			}
			// A repeat within one entry is legitimate: the mutually exclusive
			// branches of a ternary may declare the same name.
			if prev, dup := exactBy[name]; dup && prev != p.EntryIndex {
				return nil, fmt.Errorf(
					"condition type %q is declared by more than one conditions entry (%d and %d)",
					name, prev, p.EntryIndex)
			}
			exactBy[name] = p.EntryIndex
		}
	}

	for i, types := range dependencyTypes {
		for _, typ := range types {
			if _, builtin := v1alpha1.KROBuiltinConditionTypes[typ]; builtin {
				continue
			}
			for _, p := range declares[i] {
				if p.matches(typ) {
					return nil, fmt.Errorf(
						"runtime.condition(schema, %q): a conditions entry cannot depend on a "+
							"condition type it may itself declare (matches %s) in expression %q",
						typ, p, expressions[i])
				}
			}
		}
	}

	dependsOn := make([][]ConditionDependency, len(asts))
	for i, types := range dependencyTypes {
		dependsOn[i] = resolveDependencies(i, types, patterns)
	}
	ranks, err := conditionEvalRanks(dependsOn)
	if err != nil {
		if ce := dag.AsCycleError[int](err); ce != nil {
			return nil, fmt.Errorf("conditions entries form a reference cycle: %s",
				describeCycle(ce.Cycle, declares))
		}
		return nil, err
	}

	for i, root := range asts {
		if root == nil {
			continue
		}
		if err := validateConditionAST(root, patterns, expressions[i]); err != nil {
			return nil, err
		}
	}

	return &conditionEvalPlan{Ranks: ranks, DependsOn: dependsOn}, nil
}

// resolveDependencies pairs each of entryIndex's dependencyTypes with the entries
// that could declare it. allPatterns is every entry's declarations, so a
// dependency can resolve to an entry declared later in the block.
//
// A built-in resolves to an empty DeclaredBy: it is bound before the eval loop, so
// there is nothing to order against.
func resolveDependencies(
	entryIndex int,
	dependencyTypes []string,
	allPatterns []conditionPattern,
) []ConditionDependency {
	if len(dependencyTypes) == 0 {
		return nil
	}
	out := make([]ConditionDependency, 0, len(dependencyTypes))
	for _, typ := range dependencyTypes {
		d := ConditionDependency{Type: typ}
		if _, builtin := v1alpha1.KROBuiltinConditionTypes[typ]; !builtin {
			for _, p := range allPatterns {
				if p.matches(typ) && p.EntryIndex != entryIndex {
					d.DeclaredBy = append(d.DeclaredBy, p.EntryIndex)
				}
			}
			slices.Sort(d.DeclaredBy)
			d.DeclaredBy = slices.Compact(d.DeclaredBy)
		}
		out = append(out, d)
	}
	return out
}

// conditionEvalRanks ranks each entry after every entry it depends on. Entries
// with no reference relationship keep their declaration order.
func conditionEvalRanks(dependsOn [][]ConditionDependency) ([]int, error) {
	n := len(dependsOn)
	g := dag.NewDirectedAcyclicGraph[int]()
	for i := range n {
		if err := g.AddVertex(i, i); err != nil {
			return nil, err
		}
	}

	for j, jDeps := range dependsOn {
		var deps []int
		for _, d := range jDeps {
			deps = append(deps, d.DeclaredBy...)
		}
		slices.Sort(deps)
		deps = slices.Compact(deps)
		if err := g.AddDependencies(j, deps); err != nil {
			return nil, err
		}
	}

	order, err := g.TopologicalSort() // order[position] = entry index
	if err != nil {
		return nil, err
	}

	ranks := make([]int, n)
	for pos, entry := range order {
		ranks[entry] = pos
	}
	return ranks, nil
}

// describeCycle renders a cycle as a path of declaration patterns, sorted for
// stable output.
func describeCycle(cycle []int, declares [][]conditionPattern) string {
	parts := make([]string, len(cycle))
	for k, idx := range cycle {
		names := make([]string, 0, len(declares[idx]))
		for _, p := range declares[idx] {
			names = append(names, p.String())
		}
		slices.Sort(names)
		parts[k] = fmt.Sprintf("entry %d (%s)", idx, strings.Join(names, "/"))
	}
	return strings.Join(parts, " -> ")
}

// validateConditionAST applies the lookup and literal-value rules to every
// runtime call under expr, returning the first error found.
func validateConditionAST(expr celast.Expr, patterns []conditionPattern, exprText string) error {
	var firstErr error
	celast.PreOrderVisit(expr, celast.NewExprVisitor(func(e celast.Expr) {
		if firstErr != nil {
			return
		}
		switch {
		case isRuntimeCall(e, "condition"):
			firstErr = validateConditionLookup(e, patterns, exprText)
		case isRuntimeCall(e, "newCondition"):
			firstErr = validateNewConditionLiterals(e, exprText)
		}
	}))
	return firstErr
}

// validateNewConditionLiterals checks runtime.newCondition's literal status
// and type values. Computed values are checked at evaluation time.
func validateNewConditionLiterals(call celast.Expr, exprText string) error {
	args := call.AsCall().Args()
	if len(args) != 1 {
		return nil // Wrong arity is a CEL type-checker concern.
	}
	if v, ok := mapLiteralEntryValue(args[0], "status"); ok {
		if status, ok := literalString(v); ok && !library.IsValidConditionStatus(status) {
			return fmt.Errorf(
				"runtime.newCondition: status must be one of True, False, Unknown (got %q) in expression %q",
				status, exprText,
			)
		}
	}
	if v, ok := mapLiteralEntryValue(args[0], "type"); ok {
		if typeVal, ok := literalString(v); ok && typeVal == "" {
			return fmt.Errorf("runtime.newCondition: type must not be empty in expression %q", exprText)
		}
	}
	return nil
}

// validateConditionLookup enforces the lookup rules for
// runtime.condition(schema, 'X') calls: X must be a kro built-in type or a type
// some conditions entry could declare, anything else is an unknown name (typo).
// Lookups on other objects (child resources) and non-literal lookups are
// unrestricted.
func validateConditionLookup(call celast.Expr, patterns []conditionPattern, exprText string) error {
	args := call.AsCall().Args()
	if len(args) != 2 {
		return nil // Wrong arity is a CEL type-checker concern.
	}
	if obj := args[0]; obj.Kind() != celast.IdentKind || obj.AsIdent() != SchemaVarName {
		return nil
	}
	typeName, ok := literalString(args[1])
	if !ok {
		return nil
	}

	if _, isBuiltin := v1alpha1.KROBuiltinConditionTypes[typeName]; isBuiltin {
		return nil
	}

	for _, p := range patterns {
		if p.matches(typeName) {
			return nil
		}
	}
	return fmt.Errorf(
		"runtime.condition(schema, %q): unknown condition type; expected a kro built-in type "+
			"(InstanceManaged, GraphResolved, ResourcesReady, Ready) or a type declared by "+
			"another conditions entry, in expression %q",
		typeName, exprText,
	)
}

// isRuntimeCall reports whether expr is a call of the form
// runtime.<methodName>(...).
func isRuntimeCall(expr celast.Expr, methodName string) bool {
	if expr.Kind() != celast.CallKind {
		return false
	}
	call := expr.AsCall()
	if !call.IsMemberFunction() || call.FunctionName() != methodName {
		return false
	}
	target := call.Target()
	return target != nil && target.Kind() == celast.IdentKind && target.AsIdent() == library.RuntimeVarName
}

// literalString returns the string value of a literal-string AST node.
func literalString(expr celast.Expr) (string, bool) {
	if expr.Kind() != celast.LiteralKind {
		return "", false
	}
	s, ok := expr.AsLiteral().Value().(string)
	return s, ok
}

// mapLiteralEntryValue looks up key in a map literal's entries and
// returns the entry's value expression.
func mapLiteralEntryValue(expr celast.Expr, key string) (celast.Expr, bool) {
	if expr.Kind() != celast.MapKind {
		return nil, false
	}
	for _, entry := range expr.AsMap().Entries() {
		me := entry.AsMapEntry()
		if k, ok := literalString(me.Key()); ok && k == key {
			return me.Value(), true
		}
	}
	return nil, false
}
