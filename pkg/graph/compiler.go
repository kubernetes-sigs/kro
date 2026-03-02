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
	"fmt"
	"net/http"

	"k8s.io/client-go/rest"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// Parser converts a ResourceGraphDefinition into the internal parsed form.
// It is responsible for schema-agnostic decoding/normalization.
type Parser interface {
	// Parse decodes templates/status payloads and condition/forEach expressions
	// into ParsedRGD, including schemaless resource/status field extraction.
	// Parse is schema-agnostic: it does not do API discovery or type checking.
	Parse(*v1alpha1.ResourceGraphDefinition) (*ParsedRGD, error)
}

// Validator enforces structural and semantic invariants on parsed data.
// It checks rules that do not require discovery-derived schema information.
type Validator interface {
	// Validate returns an error when ParsedRGD violates shape, naming, or CEL
	// usage rules that can be checked without cluster discovery.
	Validate(*ParsedRGD) error
}

// Resolver enriches parsed nodes with discovery-derived identity and schemas.
// It resolves resources to concrete API identity and OpenAPI shape.
type Resolver interface {
	// Resolve maps parsed resources to concrete GVK/GVR/schema data and returns
	// a ResolvedRGD that is ready for reference linking and type checking.
	Resolve(*ParsedRGD) (*ResolvedRGD, error)
}

// Linker resolves expression references and computes dependency edges.
// It produces a linked graph annotated with dependency ordering data.
type Linker interface {
	// Link validates reference scopes, records node dependencies, and produces
	// DAG/topological-order data for downstream stages.
	Link(*ResolvedRGD) (*LinkedRGD, error)
}

// TypeChecker performs static type checks over linked CEL expressions.
// It validates expression inputs/outputs against resolved schemas.
type TypeChecker interface {
	// Check validates expression result/operand types against resource schemas
	// and returns a TypeCheckResult used by the program generator.
	Check(*LinkedRGD) (*TypeCheckResult, error)
}

// ProgramGenerator turns linked expressions into executable CEL programs.
// It preserves graph metadata needed by runtime evaluation.
type ProgramGenerator interface {
	// Generate emits CompiledRGD with executable programs and runtime metadata.
	// It assumes linking and type-checking contracts have already been satisfied.
	Generate(*LinkedRGD, *TypeCheckResult) (*CompiledRGD, error)
}

// Assembler materializes the final runtime Graph from compiled artifacts.
// It finalizes runtime indexes and synthesized CRD state.
type Assembler interface {
	// Assemble materializes the final Graph, including instance/node indexes and
	// synthesized CRD state consumed by runtime/controller layers.
	Assemble(*CompiledRGD) (*Graph, error)
}

// Compiler compiles a ResourceGraphDefinition into an executable graph.
type Compiler interface {
	Compile(*v1alpha1.ResourceGraphDefinition) (*Graph, error)
}

// GraphCompiler orchestrates the graph compilation pipeline end-to-end.
// Each stage can be replaced through options for testing or custom behavior.
type GraphCompiler struct {
	parser           Parser
	validator        Validator
	resolver         Resolver
	linker           Linker
	typechecker      TypeChecker
	programGenerator ProgramGenerator
	assembler        Assembler
	rgdConfig        RGDConfig
}

const (
	defaultMaxCollectionSize          = 1000
	defaultMaxCollectionDimensionSize = 10
)

func defaultRGDConfig() RGDConfig {
	return RGDConfig{
		MaxCollectionSize:          defaultMaxCollectionSize,
		MaxCollectionDimensionSize: defaultMaxCollectionDimensionSize,
	}
}

// RGDConfig configures collection guardrails across build/runtime.
type RGDConfig struct {
	MaxCollectionSize          int
	MaxCollectionDimensionSize int
}

// Option mutates GraphCompiler stage wiring before defaults are applied.
// Use options to inject custom implementations for any stage.
type Option func(*GraphCompiler)

// WithParser overrides the parser stage implementation.
func WithParser(p Parser) Option { return func(b *GraphCompiler) { b.parser = p } }

// WithValidator overrides the validator stage implementation.
func WithValidator(v Validator) Option { return func(b *GraphCompiler) { b.validator = v } }

// WithResolver overrides the resolver stage implementation.
func WithResolver(r Resolver) Option { return func(b *GraphCompiler) { b.resolver = r } }

// WithLinker overrides the linker stage implementation.
func WithLinker(l Linker) Option { return func(b *GraphCompiler) { b.linker = l } }

// WithTypeChecker overrides the type-checker stage implementation.
func WithTypeChecker(tc TypeChecker) Option { return func(b *GraphCompiler) { b.typechecker = tc } }

// WithProgramGenerator overrides the program-generator stage implementation.
func WithProgramGenerator(pg ProgramGenerator) Option {
	return func(b *GraphCompiler) { b.programGenerator = pg }
}

// WithAssembler overrides the assembler stage implementation.
func WithAssembler(a Assembler) Option { return func(b *GraphCompiler) { b.assembler = a } }

// WithRGDConfig overrides default collection guardrails used by the
// default validator/runtime pipeline stages.
func WithRGDConfig(cfg RGDConfig) Option { return func(b *GraphCompiler) { b.rgdConfig = cfg } }

// NewCompiler constructs a GraphCompiler pipeline.
//
// Configuration flow:
// 1. Apply opts to inject custom stages.
// 2. Fill any nil stage with the package default implementation.
//
// The default resolver needs config/httpClient for API discovery.
// Supplying WithResolver(...) skips default resolver construction.
func NewCompiler(config *rest.Config, httpClient *http.Client, opts ...Option) (*GraphCompiler, error) {
	b := &GraphCompiler{rgdConfig: defaultRGDConfig()}
	for _, opt := range opts {
		opt(b)
	}

	// Fill defaults only for stages not explicitly provided by options.
	if b.parser == nil {
		b.parser = newParser()
	}
	if b.validator == nil {
		b.validator = newValidatorWithConfig(b.rgdConfig)
	}
	if b.resolver == nil {
		r, err := newResolver(config, httpClient)
		if err != nil {
			return nil, fmt.Errorf("create resolver: %w", err)
		}
		b.resolver = r
	}
	if b.linker == nil {
		b.linker = newLinker()
	}
	if b.typechecker == nil {
		b.typechecker = newTypeChecker()
	}
	if b.programGenerator == nil {
		b.programGenerator = newProgramGenerator()
	}
	if b.assembler == nil {
		b.assembler = newAssembler()
	}

	return b, nil
}

// Compile compiles a ResourceGraphDefinition into a Graph.
//
//	Parse -> Validate -> Resolve -> Link -> TypeCheck -> Compile -> Assemble
func (b *GraphCompiler) Compile(rgd *v1alpha1.ResourceGraphDefinition) (*Graph, error) {
	parsed, err := b.parser.Parse(rgd)
	if err != nil {
		return nil, err
	}
	if err := b.validator.Validate(parsed); err != nil {
		return nil, err
	}
	resolved, err := b.resolver.Resolve(parsed)
	if err != nil {
		return nil, err
	}
	linked, err := b.linker.Link(resolved)
	if err != nil {
		return nil, err
	}
	checked, err := b.typechecker.Check(linked)
	if err != nil {
		return nil, err
	}
	compiled, err := b.programGenerator.Generate(linked, checked)
	if err != nil {
		return nil, err
	}
	return b.assembler.Assemble(compiled)
}
