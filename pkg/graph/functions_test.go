package graph

import (
	"testing"

	"github.com/google/cel-go/common/decls"
	"github.com/kro-run/kro/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildFunctions(t *testing.T) {
	tests := []struct {
		name            string
		functions       []v1alpha1.FunctionDefinition
		wantErr         bool
		errMsgSubstring string
		checkFuncs      func(t *testing.T, funcs []*decls.FunctionDecl)
	}{
		{
			name: "valid function with inputs and return type",
			functions: []v1alpha1.FunctionDefinition{
				{
					Name: "add",
					Inputs: []v1alpha1.FunctionInput{
						{Name: "a", Type: "int"},
						{Name: "b", Type: "int"},
					},
					ReturnType:    "int",
					CELExpression: "a + b",
				},
			},
			wantErr: false,
			checkFuncs: func(t *testing.T, funcs []*decls.FunctionDecl) {
				require.Len(t, funcs, 1)
				assert.Equal(t, "add", funcs[0].Name())
			},
		},
		{
			name: "valid function no inputs, inferred return type",
			functions: []v1alpha1.FunctionDefinition{
				{
					Name:          "get_true",
					CELExpression: "true",
				},
			},
			wantErr: false,
			checkFuncs: func(t *testing.T, funcs []*decls.FunctionDecl) {
				require.Len(t, funcs, 1)
				assert.Equal(t, "get_true", funcs[0].Name())
			},
		},
		{
			name: "valid function with default input name",
			functions: []v1alpha1.FunctionDefinition{
				{
					Name: "identity_str",
					Inputs: []v1alpha1.FunctionInput{
						{Type: "string"}, // Name intentionally omitted
					},
					ReturnType:    "string",
					CELExpression: "_0", // Default name for first input
				},
			},
			wantErr: false,
			checkFuncs: func(t *testing.T, funcs []*decls.FunctionDecl) {
				require.Len(t, funcs, 1)
				assert.Equal(t, "identity_str", funcs[0].Name())
			},
		},
		{
			name: "valid function, multiple inputs, complex expression",
			functions: []v1alpha1.FunctionDefinition{
				{
					Name: "complex_logic",
					Inputs: []v1alpha1.FunctionInput{
						{Name: "x", Type: "int"},
						{Name: "y", Type: "string"},
					},
					ReturnType:    "bool",
					CELExpression: "x > 10 && y.contains('test')",
				},
			},
			wantErr: false,
		},
		{
			name:      "empty function list",
			functions: []v1alpha1.FunctionDefinition{},
			wantErr:   false,
			checkFuncs: func(t *testing.T, funcs []*decls.FunctionDecl) {
				assert.Empty(t, funcs)
			},
		},
		{
			name: "invalid input type",
			functions: []v1alpha1.FunctionDefinition{
				{
					Name: "bad_input",
					Inputs: []v1alpha1.FunctionInput{
						{Name: "a", Type: "nonExistentType"},
					},
					CELExpression: "a",
				},
			},
			wantErr:         true,
			errMsgSubstring: "unsupported CEL type: nonExistentType",
		},
		{
			name: "invalid return type string",
			functions: []v1alpha1.FunctionDefinition{
				{
					Name:          "bad_return_type",
					ReturnType:    "nonExistentType",
					CELExpression: "true",
				},
			},
			wantErr:         true,
			errMsgSubstring: "unsupported CEL type: nonExistentType",
		},
		{
			name: "CEL expression compilation error - syntax",
			functions: []v1alpha1.FunctionDefinition{
				{
					Name:          "compile_err_syntax",
					CELExpression: "a + ",
				},
			},
			wantErr:         true,
			errMsgSubstring: "failed to parse expression",
		},
		{
			name: "return type mismatch",
			functions: []v1alpha1.FunctionDefinition{
				{
					Name:          "return_mismatch",
					ReturnType:    "string",
					CELExpression: "123",
				},
			},
			wantErr:         true,
			errMsgSubstring: "return type mismatch: expected string, got int",
		},
		{
			name: "undeclared variable in CEL expression",
			functions: []v1alpha1.FunctionDefinition{
				{
					Name: "undeclared_var",
					Inputs: []v1alpha1.FunctionInput{
						{Name: "a", Type: "int"},
					},
					CELExpression: "a + b", // b is not defined
				},
			},
			wantErr:         true,
			errMsgSubstring: "undeclared reference to 'b'",
		},
		{
			name: "invalid function ID pattern",
			functions: []v1alpha1.FunctionDefinition{
				{
					Name:          "invalid$id",
					CELExpression: "true",
				},
			},
			wantErr:         true,
			errMsgSubstring: "function \"invalid$id\": name is invalid",
		},
		{
			name: "input name clashes with CEL built-in",
			functions: []v1alpha1.FunctionDefinition{
				{
					Name: "clash_input",
					Inputs: []v1alpha1.FunctionInput{
						{Name: "size", Type: "string"}, // 'size' is a common CEL function
					},
					CELExpression: "size == 'test'", // Tries to use the input 'size'
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			processedFuncs, err := buildFunctions(tt.functions)

			if tt.wantErr {
				assert.Error(t, err)
				if tt.errMsgSubstring != "" {
					assert.Contains(t, err.Error(), tt.errMsgSubstring)
				}
			} else {
				assert.NoError(t, err)
				if tt.checkFuncs != nil {
					tt.checkFuncs(t, processedFuncs)
				}
			}
		})
	}
}
