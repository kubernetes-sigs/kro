// Copyright 2025 The Kubernetes Authors.
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
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

type flowParserFunc func(*v1alpha1.ResourceGraphDefinition) (*ParsedRGD, error)

func (f flowParserFunc) Parse(rgd *v1alpha1.ResourceGraphDefinition) (*ParsedRGD, error) {
	return f(rgd)
}

type flowValidatorFunc func(*ParsedRGD) error

func (f flowValidatorFunc) Validate(parsed *ParsedRGD) error { return f(parsed) }

type flowResolverFunc func(*ParsedRGD) (*ResolvedRGD, error)

func (f flowResolverFunc) Resolve(parsed *ParsedRGD) (*ResolvedRGD, error) { return f(parsed) }

type flowLinkerFunc func(*ResolvedRGD) (*LinkedRGD, error)

func (f flowLinkerFunc) Link(resolved *ResolvedRGD) (*LinkedRGD, error) { return f(resolved) }

type flowTypeCheckerFunc func(*LinkedRGD) (*TypeCheckResult, error)

func (f flowTypeCheckerFunc) Check(linked *LinkedRGD) (*TypeCheckResult, error) { return f(linked) }

type flowProgramGeneratorFunc func(*LinkedRGD, *TypeCheckResult) (*CompiledRGD, error)

func (f flowProgramGeneratorFunc) Generate(linked *LinkedRGD, tc *TypeCheckResult) (*CompiledRGD, error) {
	return f(linked, tc)
}

type flowAssemblerFunc func(*CompiledRGD) (*Graph, error)

func (f flowAssemblerFunc) Assemble(compiled *CompiledRGD) (*Graph, error) { return f(compiled) }

func TestNewCompiler_Cases(t *testing.T) {
	tests := []struct {
		name    string
		config  *rest.Config
		client  *http.Client
		wantErr string
	}{
		{
			name:   "initializes defaults",
			config: &rest.Config{},
			client: &http.Client{},
		},
		{
			name:    "schema resolver creation failure",
			config:  &rest.Config{Host: "http://[::1"},
			client:  &http.Client{},
			wantErr: "create resolver: schema resolver",
		},
		{
			name:    "rest mapper creation failure",
			config:  &rest.Config{},
			client:  nil,
			wantErr: "create resolver: REST mapper",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewCompiler(tt.config, tt.client)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				require.Nil(t, got)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, got)
			require.NotNil(t, got.parser)
			require.NotNil(t, got.validator)
			require.NotNil(t, got.resolver)
			require.NotNil(t, got.linker)
			require.NotNil(t, got.typechecker)
			require.NotNil(t, got.programGenerator)
			require.NotNil(t, got.assembler)
		})
	}
}

func TestGraphCompiler_Compile_FlowOrder_Case(t *testing.T) {
	var calls []string

	parsed := &ParsedRGD{}
	resolved := &ResolvedRGD{}
	linked := &LinkedRGD{}
	checked := &TypeCheckResult{}
	compiled := &CompiledRGD{}
	expectedGraph := &Graph{}

	b := &GraphCompiler{
		parser: flowParserFunc(func(*v1alpha1.ResourceGraphDefinition) (*ParsedRGD, error) {
			calls = append(calls, "parse")
			return parsed, nil
		}),
		validator: flowValidatorFunc(func(got *ParsedRGD) error {
			calls = append(calls, "validate")
			require.Same(t, parsed, got)
			return nil
		}),
		resolver: flowResolverFunc(func(got *ParsedRGD) (*ResolvedRGD, error) {
			calls = append(calls, "resolve")
			require.Same(t, parsed, got)
			return resolved, nil
		}),
		linker: flowLinkerFunc(func(got *ResolvedRGD) (*LinkedRGD, error) {
			calls = append(calls, "link")
			require.Same(t, resolved, got)
			return linked, nil
		}),
		typechecker: flowTypeCheckerFunc(func(got *LinkedRGD) (*TypeCheckResult, error) {
			calls = append(calls, "typecheck")
			require.Same(t, linked, got)
			return checked, nil
		}),
		programGenerator: flowProgramGeneratorFunc(func(gotLinked *LinkedRGD, gotChecked *TypeCheckResult) (*CompiledRGD, error) {
			calls = append(calls, "compile")
			require.Same(t, linked, gotLinked)
			require.Same(t, checked, gotChecked)
			return compiled, nil
		}),
		assembler: flowAssemblerFunc(func(got *CompiledRGD) (*Graph, error) {
			calls = append(calls, "assemble")
			require.Same(t, compiled, got)
			return expectedGraph, nil
		}),
	}

	got, err := b.Compile(&v1alpha1.ResourceGraphDefinition{})
	require.NoError(t, err)
	require.Same(t, expectedGraph, got)
	require.Equal(t, []string{"parse", "validate", "resolve", "link", "typecheck", "compile", "assemble"}, calls)
}

func TestGraphCompiler_Compile_Error_Case(t *testing.T) {
	compileErr := errors.New("compile failed")
	assembleCalled := false

	b := &GraphCompiler{
		parser: flowParserFunc(func(*v1alpha1.ResourceGraphDefinition) (*ParsedRGD, error) {
			return &ParsedRGD{}, nil
		}),
		validator: flowValidatorFunc(func(*ParsedRGD) error { return nil }),
		resolver: flowResolverFunc(func(*ParsedRGD) (*ResolvedRGD, error) {
			return &ResolvedRGD{}, nil
		}),
		linker: flowLinkerFunc(func(*ResolvedRGD) (*LinkedRGD, error) {
			return &LinkedRGD{}, nil
		}),
		typechecker: flowTypeCheckerFunc(func(*LinkedRGD) (*TypeCheckResult, error) {
			return &TypeCheckResult{}, nil
		}),
		programGenerator: flowProgramGeneratorFunc(func(*LinkedRGD, *TypeCheckResult) (*CompiledRGD, error) {
			return nil, compileErr
		}),
		assembler: flowAssemblerFunc(func(*CompiledRGD) (*Graph, error) {
			assembleCalled = true
			return &Graph{}, nil
		}),
	}

	got, err := b.Compile(&v1alpha1.ResourceGraphDefinition{})
	require.ErrorIs(t, err, compileErr)
	require.Nil(t, got)
	require.False(t, assembleCalled)
}

func TestFieldKind_String_CoverageCases(t *testing.T) {
	tests := []struct {
		name string
		kind FieldKind
		want string
	}{
		{name: "static", kind: FieldStatic, want: "Static"},
		{name: "dynamic", kind: FieldDynamic, want: "Dynamic"},
		{name: "iteration", kind: FieldIteration, want: "Iteration"},
		{name: "unknown", kind: FieldKind(99), want: "FieldKind(99)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.kind.String())
		})
	}
}

func TestNodeType_String_CoverageCases(t *testing.T) {
	tests := []struct {
		name string
		typ  NodeType
		want string
	}{
		{name: "scalar", typ: NodeTypeScalar, want: "Scalar"},
		{name: "collection", typ: NodeTypeCollection, want: "Collection"},
		{name: "external", typ: NodeTypeExternal, want: "External"},
		{name: "instance", typ: NodeTypeInstance, want: "Instance"},
		{name: "unknown", typ: NodeType(99), want: "NodeType(99)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.typ.String())
		})
	}
}

func TestGraphCompiler_OptionFunctions_CoverageCases(t *testing.T) {
	p := flowParserFunc(func(*v1alpha1.ResourceGraphDefinition) (*ParsedRGD, error) { return &ParsedRGD{}, nil })
	v := flowValidatorFunc(func(*ParsedRGD) error { return nil })
	r := flowResolverFunc(func(*ParsedRGD) (*ResolvedRGD, error) { return &ResolvedRGD{}, nil })
	l := flowLinkerFunc(func(*ResolvedRGD) (*LinkedRGD, error) { return &LinkedRGD{}, nil })
	tc := flowTypeCheckerFunc(func(*LinkedRGD) (*TypeCheckResult, error) { return &TypeCheckResult{}, nil })
	c := flowProgramGeneratorFunc(func(*LinkedRGD, *TypeCheckResult) (*CompiledRGD, error) {
		return &CompiledRGD{}, nil
	})
	a := flowAssemblerFunc(func(*CompiledRGD) (*Graph, error) { return &Graph{}, nil })
	cfg := RGDConfig{MaxCollectionSize: 7, MaxCollectionDimensionSize: 3}

	b := &GraphCompiler{}
	WithParser(p)(b)
	WithValidator(v)(b)
	WithResolver(r)(b)
	WithLinker(l)(b)
	WithTypeChecker(tc)(b)
	WithProgramGenerator(c)(b)
	WithAssembler(a)(b)
	WithRGDConfig(cfg)(b)

	require.NotNil(t, b.parser)
	require.NotNil(t, b.validator)
	require.NotNil(t, b.resolver)
	require.NotNil(t, b.linker)
	require.NotNil(t, b.typechecker)
	require.NotNil(t, b.programGenerator)
	require.NotNil(t, b.assembler)
	require.Equal(t, cfg, b.rgdConfig)
}

func TestNewCompiler_WithOptions_CoverageCase(t *testing.T) {
	p := flowParserFunc(func(*v1alpha1.ResourceGraphDefinition) (*ParsedRGD, error) { return &ParsedRGD{}, nil })
	v := flowValidatorFunc(func(*ParsedRGD) error { return nil })
	r := flowResolverFunc(func(*ParsedRGD) (*ResolvedRGD, error) { return &ResolvedRGD{}, nil })
	l := flowLinkerFunc(func(*ResolvedRGD) (*LinkedRGD, error) { return &LinkedRGD{}, nil })
	tc := flowTypeCheckerFunc(func(*LinkedRGD) (*TypeCheckResult, error) { return &TypeCheckResult{}, nil })
	c := flowProgramGeneratorFunc(func(*LinkedRGD, *TypeCheckResult) (*CompiledRGD, error) {
		return &CompiledRGD{}, nil
	})
	a := flowAssemblerFunc(func(*CompiledRGD) (*Graph, error) { return &Graph{}, nil })

	got, err := NewCompiler(
		&rest.Config{},
		&http.Client{},
		WithParser(p),
		WithValidator(v),
		WithResolver(r),
		WithLinker(l),
		WithTypeChecker(tc),
		WithProgramGenerator(c),
		WithAssembler(a),
	)
	require.NoError(t, err)
	require.NotNil(t, got)
}
