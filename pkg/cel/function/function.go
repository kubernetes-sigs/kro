package function

import (
	"fmt"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	xv1alpha1 "github.com/kro-run/kro/api/v1alpha1"
)

// FunctionRegistry manages custom CEL functions
type FunctionRegistry struct {
	functions map[string]*xv1alpha1.Function
}

// NewFunctionRegistry creates a new function registry
func NewFunctionRegistry() *FunctionRegistry {
	return &FunctionRegistry{
		functions: make(map[string]*xv1alpha1.Function),
	}
}

// RegisterFunction adds a function to the registry
func (r *FunctionRegistry) RegisterFunction(fn *xv1alpha1.Function) error {
	if _, exists := r.functions[fn.ID]; exists {
		return fmt.Errorf("function %s already registered", fn.ID)
	}
	r.functions[fn.ID] = fn
	return nil
}

// GetCELFunction converts a Function to a CEL function declaration
func (r *FunctionRegistry) GetCELFunction(fn *xv1alpha1.Function) (cel.EnvOption, error) {

	//convert input types to CEL types
	celTypes := make([]*cel.Type, len((fn.Inputs)))
	envOpts := make([]cel.EnvOption, len(fn.Inputs))

	for i, inputType := range fn.Inputs {
		celType, err := getCelType(inputType)
		if err != nil {
			return nil, err
		}
		celTypes[i] = celType
		envOpts[i] = cel.Variable(fmt.Sprintf("_%d", i), celType)
	}

	// Create CEL environment for function template
	env, err := cel.NewEnv(envOpts...)
	if err != nil {
		return nil, fmt.Errorf("environment creation failed: %w", err)
	}

	// Compile function template
	ast, iss := env.Compile(fn.Template)
	if iss.Err() != nil {
		return nil, fmt.Errorf("template compilation failed: %w", iss.Err())
	}

	prg, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("program creation failed: %w", err)
	}

	// Return CEL function declaration
	return cel.Function(fn.ID,
		cel.Overload(fn.ID,
			celTypes,
			ast.OutputType(),
			cel.FunctionBinding(func(args ...ref.Val) ref.Val {
				vars := map[string]any{}
				for i, arg := range args {
					vars[fmt.Sprintf("_%d", i)] = arg
				}
				out, _, err := prg.Eval(vars)
				if err != nil {
					return types.WrapErr(err)
				}
				return out
			}),
		),
	), nil

}

func getCelType(typeName string) (*cel.Type, error) {
	switch typeName {
	case "string":
		return cel.StringType, nil
	case "int":
		return cel.IntType, nil
	case "bool":
		return cel.BoolType, nil
	case "double", "float":
		return cel.DoubleType, nil
	case "bytes":
		return cel.BytesType, nil
	case "any":
		return cel.AnyType, nil
	case "list":
		return cel.ListType(cel.AnyType), nil
	case "map":
		return cel.MapType(cel.StringType, cel.AnyType), nil
	default:
		return nil, fmt.Errorf("unsupported type: %s", typeName)
	}
}
