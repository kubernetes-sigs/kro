// timesolver.go implements compile-time detection of solvable time comparison
// expressions and runtime threshold evaluation for precise requeue scheduling.
//
// When a CEL expression like `time.now() - timestamp(X) >= duration('2h')`
// evaluates to false, the solver computes the exact moment it will become true
// (X + 2h) and returns the duration until that moment as a requeue hint.
//
// Detection happens at compile time via AST pattern matching. Evaluation of
// the threshold sub-expression happens at runtime against the current scope.
package compiler

import (
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/parser"
)

// TimeComparison holds compile-time metadata for a solvable time expression.
// The threshold program evaluates to the timestamp at which the comparison flips.
type TimeComparison struct {
	// ThresholdProgram evaluates to the timestamp at which the comparison
	// becomes true. For `time.now() - E >= D`, this computes `E + D`.
	ThresholdProgram cel.Program
}

// analyzeTimeComparisons walks compiled expressions and identifies solvable
// time comparison patterns. Returns a map from expression string to metadata.
// Only expressions matching supported patterns get entries.
func analyzeTimeComparisons(env *cel.Env, compiledExprs map[string]*cel.Ast) map[string]*TimeComparison {
	result := make(map[string]*TimeComparison)
	for exprStr, checked := range compiledExprs {
		if tc := analyzeTimeExpr(env, checked); tc != nil {
			result[exprStr] = tc
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// analyzeTimeExpr checks if an expression contains a solvable time pattern
// anywhere in its AST. It recursively walks the tree to find patterns nested
// inside list literals, function arguments, ternaries, etc.
//
// Supported patterns:
//   - time.now() - E >= D  → threshold = E + D
//   - time.now() - E > D   → threshold = E + D
//   - (time.now() - E >= D) ? X : Y  → threshold = E + D (ternary wrapping)
//
// Where E is any timestamp sub-expression and D is any duration sub-expression.
func analyzeTimeExpr(env *cel.Env, checked *cel.Ast) *TimeComparison {
	expr := checked.NativeRep().Expr()
	return findTimeComparison(env, expr)
}

// findTimeComparison recursively walks the AST looking for a solvable
// time comparison pattern. Returns the first one found.
func findTimeComparison(env *cel.Env, expr ast.Expr) *TimeComparison {
	if expr == nil {
		return nil
	}

	switch expr.Kind() {
	case ast.CallKind:
		call := expr.AsCall()
		fn := call.FunctionName()

		// Check if THIS node is a solvable comparison.
		if isComparisonOp(fn) {
			if tc := tryBuildThreshold(env, call.Args()); tc != nil {
				return tc
			}
		}

		// Check if this is a ternary with a solvable condition.
		if fn == operators.Conditional && len(call.Args()) == 3 {
			condExpr := call.Args()[0]
			if condExpr.Kind() == ast.CallKind {
				condCall := condExpr.AsCall()
				if isComparisonOp(condCall.FunctionName()) {
					if tc := tryBuildThreshold(env, condCall.Args()); tc != nil {
						return tc
					}
				}
			}
		}

		// Recurse into target and all arguments.
		if call.IsMemberFunction() && call.Target() != nil {
			if tc := findTimeComparison(env, call.Target()); tc != nil {
				return tc
			}
		}
		for _, arg := range call.Args() {
			if tc := findTimeComparison(env, arg); tc != nil {
				return tc
			}
		}

	case ast.ListKind:
		list := expr.AsList()
		for _, elem := range list.Elements() {
			if tc := findTimeComparison(env, elem); tc != nil {
				return tc
			}
		}

	case ast.MapKind:
		m := expr.AsMap()
		for _, entry := range m.Entries() {
			mapEntry := entry.AsMapEntry()
			if tc := findTimeComparison(env, mapEntry.Key()); tc != nil {
				return tc
			}
			if tc := findTimeComparison(env, mapEntry.Value()); tc != nil {
				return tc
			}
		}

	case ast.StructKind:
		s := expr.AsStruct()
		for _, field := range s.Fields() {
			sf := field.AsStructField()
			if tc := findTimeComparison(env, sf.Value()); tc != nil {
				return tc
			}
		}

	case ast.SelectKind:
		sel := expr.AsSelect()
		if tc := findTimeComparison(env, sel.Operand()); tc != nil {
			return tc
		}

	case ast.ComprehensionKind:
		comp := expr.AsComprehension()
		if tc := findTimeComparison(env, comp.IterRange()); tc != nil {
			return tc
		}
		if tc := findTimeComparison(env, comp.LoopStep()); tc != nil {
			return tc
		}
		if tc := findTimeComparison(env, comp.Result()); tc != nil {
			return tc
		}
	}

	return nil
}

// isComparisonOp returns true for comparison operators where time flows
// forward past a threshold.
func isComparisonOp(fn string) bool {
	return fn == operators.GreaterEquals || fn == operators.Greater ||
		fn == operators.LessEquals || fn == operators.Less
}

// tryBuildThreshold checks if the comparison args match time.now() - E >= D
// and builds a threshold program.
func tryBuildThreshold(env *cel.Env, args []ast.Expr) *TimeComparison {
	if len(args) != 2 {
		return nil
	}
	lhs, rhs := args[0], args[1]

	// LHS must be subtraction: time.now() - E
	if lhs.Kind() != ast.CallKind {
		return nil
	}
	lhsCall := lhs.AsCall()
	if lhsCall.FunctionName() != operators.Subtract {
		return nil
	}
	subArgs := lhsCall.Args()
	if len(subArgs) != 2 {
		return nil
	}

	// First arg of subtract must be time.now()
	if !isTimeNowCall(subArgs[0]) {
		return nil
	}

	// subArgs[1] is E (the timestamp expression)
	// rhs is D (the duration expression)
	// Threshold = E + D
	return buildThresholdProgram(env, subArgs[1], rhs)
}

// buildThresholdProgram constructs a CEL program that evaluates E + D,
// representing the timestamp at which the comparison becomes true.
func buildThresholdProgram(env *cel.Env, timestampExpr, durationExpr ast.Expr) *TimeComparison {
	eStr, err := parser.Unparse(timestampExpr, nil)
	if err != nil {
		return nil
	}
	dStr, err := parser.Unparse(durationExpr, nil)
	if err != nil {
		return nil
	}

	thresholdExpr := eStr + " + " + dStr

	// Compile the threshold expression in the same environment.
	thresholdAst, iss := env.Parse(thresholdExpr)
	if iss != nil && iss.Err() != nil {
		return nil
	}
	thresholdChecked, iss := env.Check(thresholdAst)
	if iss != nil && iss.Err() != nil {
		// Type-check may fail for dyn-typed variables; try program without check.
		prg, err := env.Program(thresholdAst)
		if err != nil {
			return nil
		}
		return &TimeComparison{ThresholdProgram: prg}
	}
	prg, err2 := env.Program(thresholdChecked)
	if err2 != nil {
		return nil
	}
	return &TimeComparison{ThresholdProgram: prg}
}

// isTimeNowCall returns true if the expression is a call to time.now() with no arguments.
func isTimeNowCall(e ast.Expr) bool {
	if e.Kind() != ast.CallKind {
		return false
	}
	c := e.AsCall()
	return c.FunctionName() == "time.now" && len(c.Args()) == 0 && !c.IsMemberFunction()
}

// SolveTimeComparison evaluates a time comparison's threshold against the
// current scope and returns how long until the comparison becomes true.
// Returns 0 if the threshold is already past or evaluation fails.
func (c *CompiledGraph) SolveTimeComparison(expr string, scope map[string]any) time.Duration {
	tc, ok := c.TimeComparisons[expr]
	if !ok || tc == nil {
		return 0
	}
	out, _, err := tc.ThresholdProgram.Eval(c.WrapScope(scope))
	if err != nil {
		return 0
	}
	ts, ok := out.(types.Timestamp)
	if !ok {
		return 0
	}
	d := time.Until(ts.Time)
	if d <= 0 {
		return 0
	}
	return d
}
