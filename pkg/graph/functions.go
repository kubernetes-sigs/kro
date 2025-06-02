package graph

import (
	"fmt"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/decls"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/kro-run/kro/api/v1alpha1"
	krocel "github.com/kro-run/kro/pkg/cel"
	"github.com/kro-run/kro/pkg/cel/ast"
	"github.com/kro-run/kro/pkg/graph/dag"
)

// celTypeFromString converts a string to a cel.Type
func celTypeFromString(typeName string) (*cel.Type, error) {
	switch typeName {
	case "string":
		return cel.StringType, nil
	case "int":
		return cel.IntType, nil
	case "uint":
		return cel.UintType, nil
	case "bool":
		return cel.BoolType, nil
	case "bytes":
		return cel.BytesType, nil
	case "double", "float":
		return cel.DoubleType, nil
	case "timestamp":
		return cel.TimestampType, nil
	case "duration":
		return cel.DurationType, nil
	default:
		return nil, fmt.Errorf("unsupported CEL type: %s", typeName)
	}
}

// topologicallySortFunctions topologically sorts the provided function definitions by their dependencies,
// i.e. other functions in the list.
func topologicallySortFunctions(functions []v1alpha1.FunctionDefinition) ([]v1alpha1.FunctionDefinition, error) {
	// Group functions by name to facilitate the merging of function overloads.
	nameToDefinitions := make(map[string][]v1alpha1.FunctionDefinition)
	for _, fn := range functions {
		nameToDefinitions[fn.Name] = append(nameToDefinitions[fn.Name], fn)
	}

	// Create a dummy CEL environment for the inspector.
	// This is only used to inspect the function definitions for their dependencies,
	// and since we don't need to evaluate the expressions, we can use a simple environment
	// that doesn't have all the custom declarations (placing those functions in the
	// UnknownFunctions category).
	env, err := cel.NewEnv()
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	inspector := ast.NewInspectorWithEnv(env, nil)
	signatureToName := make(map[string]string)
	g := dag.NewDirectedAcyclicGraph[string]()

	for i, f := range functions {
		sig := f.Signature()
		if _, exists := signatureToName[sig]; exists {
			return nil, fmt.Errorf("function %q: duplicate function definition %q", f.Name, sig)
		}
		signatureToName[sig] = f.Name

		// The name of the function is not unique, hence why it's necessary
		// to create vertices by using the signature as that uniquely identifies the function.
		if err := g.AddVertex(sig, i); err != nil {
			return nil, fmt.Errorf("function %q: %w", f.Name, err)
		}

		inspectionResult, err := inspector.Inspect(f.CELExpression)
		if err != nil {
			return nil, fmt.Errorf("function %q: failed to inspect expression %s: %w", f.Name, f.CELExpression, err)
		}

		for _, fn := range inspectionResult.FunctionCalls {
			// For the purposes of topologically sorting the functions, those
			// that aren't defined in the RGD are not considered. If a function
			// refers to another function that doesn't and won't exist, it will
			// be caught in a later stage.
			if fns, ok := nameToDefinitions[fn.Name]; ok {
				for _, fn := range fns {
					if err := g.AddDependencies(sig, []string{fn.Signature()}); err != nil {
						return nil, fmt.Errorf("function %q: %w", f.Name, err)
					}

				}
			}
		}
		for _, fn := range inspectionResult.UnknownFunctions {
			if fns, ok := nameToDefinitions[fn.Name]; ok {
				for _, fn := range fns {
					if err := g.AddDependencies(sig, []string{fn.Signature()}); err != nil {
						return nil, fmt.Errorf("function %q: %w", f.Name, err)
					}
				}
			}
		}
	}

	sortedSignatures, err := g.TopologicalSort()
	if err != nil {
		return nil, fmt.Errorf("failed to topologically sort functions: %w", err)
	}

	sortedDefinitions := make([]v1alpha1.FunctionDefinition, 0, len(functions))
	for _, signature := range sortedSignatures {
		sortedDefinitions = append(sortedDefinitions, nameToDefinitions[signatureToName[signature]]...)
	}

	return sortedDefinitions, nil
}

// buildFunctions validates the provided custom function definitions and converts them
// into a processed format that can be used by the CEL environment.
func buildFunctions(functions []v1alpha1.FunctionDefinition) ([]*decls.FunctionDecl, error) {
	sortedFunctions, err := topologicallySortFunctions(functions)
	if err != nil {
		return nil, err
	}

	result := make([]*decls.FunctionDecl, 0, len(sortedFunctions))
	for _, fn := range sortedFunctions {
		if !validIdentifierRegex.MatchString(fn.Name) {
			return nil, fmt.Errorf("function %q: name is invalid, it must match the regex %q", fn.Name, validIdentifierRegex.String())
		}

		// fn.CELExpression and fn.ExternalRef are mutually exclusive
		var fnDecl *decls.FunctionDecl
		if fn.ExternalRef != nil {
			return nil, fmt.Errorf("function %q: support for external ref is not yet implemented", fn.Name)
		} else if fn.CELExpression == "" {
			return nil, fmt.Errorf("function %q: CEL expression is empty", fn.Name)
		} else {
			fnDecl, err = buildCELFunction(fn, result...)
			if err != nil {
				return nil, fmt.Errorf("function %q: %w", fn.Name, err)
			}
		}

		// Merge overloads with the previous function of the same name if it exists.
		// One declaration per function name with multiple overloads is conceptually
		// easier to work with than having multiple declarations for the same function name.
		if len(result) > 0 && result[len(result)-1].Name() == fn.Name {
			merged, err := result[len(result)-1].Merge(fnDecl)
			if err != nil {
				return nil, fmt.Errorf("function %q: failed to merge with previous: %w", fn.Name, err)
			}
			result[len(result)-1] = merged
		} else {
			result = append(result, fnDecl)
		}
	}

	return result, nil
}

// buildCELFunction builds a CEL function from a function definition, and optionally other functions that it can use.
func buildCELFunction(fn v1alpha1.FunctionDefinition, funcs ...*decls.FunctionDecl) (*decls.FunctionDecl, error) {
	// Include previous function declarations so that the function can use them.
	envOpts := []cel.EnvOption{cel.FunctionDecls(funcs...)}
	parameterNames := make([]string, len(fn.Inputs))
	inputs := make([]*cel.Type, len(fn.Inputs))

	for i, input := range fn.Inputs {
		name := input.Name
		if name == "" {
			// For simple functions, the user probably doesn't care about the parameter name,
			// so one is generated for them based on the parameter index (_0, _1, _2, etc).
			name = fmt.Sprintf("_%d", i)
		}

		paramType, err := celTypeFromString(input.Type)
		if err != nil {
			return nil, err
		}

		envOpts = append(envOpts, cel.Variable(name, paramType))
		parameterNames[i] = name
		inputs[i] = paramType
	}

	// Create a CEL environment with the provided function declarations and the arguments
	// to the function, in the form of variables.
	env, err := krocel.DefaultEnvironment(krocel.WithCustomDeclarations(envOpts...))
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	ast, iss := env.Compile(fn.CELExpression)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("failed to parse expression: %w", iss.Err())
	}

	// Return type is optional, but if it's set, it must match the expected type.
	if fn.ReturnType != "" {
		if resultType, err := celTypeFromString(fn.ReturnType); err != nil {
			return nil, fmt.Errorf("return type: %w", err)
		} else if resultType != ast.OutputType() {
			return nil, fmt.Errorf("return type mismatch: expected %s, got %s", fn.ReturnType, ast.OutputType())
		}
	}

	prg, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL program: %w", err)
	}

	fun := func(args ...ref.Val) ref.Val {
		if len(args) != len(fn.Inputs) {
			return types.WrapErr(fmt.Errorf("function %q: expected %d arguments, got %d", fn.Name, len(fn.Inputs), len(args)))
		}

		vars := map[string]any{}
		for i, name := range parameterNames {
			vars[name] = args[i]
		}

		out, _, err := prg.Eval(vars)
		if err != nil {
			return types.WrapErr(err)
		}

		return out
	}

	return decls.NewFunction(
		fn.Name,
		cel.Overload(
			fn.Signature(),
			inputs,
			ast.OutputType(),
			cel.FunctionBinding(fun),
		),
	)
}
