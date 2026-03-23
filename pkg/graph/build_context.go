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

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	apiservercel "k8s.io/apiserver/pkg/cel"
	"k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/kube-openapi/pkg/validation/spec"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph/schema"
)

// buildContext holds per-build memoized state for a single
// NewResourceGraphDefinition() call. Not safe for concurrent use.
//
// Lifecycle: created as a local variable inside NewResourceGraphDefinition(),
// passed by pointer to all helper functions, and becomes unreachable when
// NewResourceGraphDefinition returns. Never stored on Builder or Graph.
// All maps and their contents are GC'd together with the buildContext.
type buildContext struct {
	env          *cel.Env
	typeProvider *krocel.DeclTypeProvider
	schemaCache  *schema.Cache

	// schema pointer → DeclType. Avoids redundant SchemaDeclTypeWithMetadata
	// calls for schemas already converted. Schema pointers are live in the
	// caller's locals for the entire build duration.
	declTypes map[*spec.Schema]*apiservercel.DeclType

	// (env pointer, expression string) → checked AST. Deduplicates the
	// status two-phase pattern where parseAndCheck runs first for type
	// inference, then compile reuses the cached AST.
	checkedASTs map[checkedASTKey]*cel.Ast

	// (parent env, varName) → extended env. Deduplicates readyWhen on
	// collection nodes that share the same schema.
	extendedEnvs map[extendedEnvKey]*cel.Env
}

type checkedASTKey struct {
	env  *cel.Env
	expr string
}

type extendedEnvKey struct {
	parent  *cel.Env
	varName string
}

// schemaDeclType returns a DeclType for the given schema, memoized by pointer.
func (bc *buildContext) schemaDeclType(s *spec.Schema) *apiservercel.DeclType {
	if s == nil {
		return nil
	}
	if dt, ok := bc.declTypes[s]; ok {
		return dt
	}
	dt := krocel.SchemaDeclTypeWithMetadata(&openapi.Schema{Schema: s}, false)
	bc.declTypes[s] = dt
	return dt
}

// parseAndCheck parses and type-checks a CEL expression, storing the checked
// AST for later reuse by compile. Does not compile a program.
func (bc *buildContext) parseAndCheck(env *cel.Env, expr *krocel.Expression) (*cel.Ast, error) {
	key := checkedASTKey{env: env, expr: expr.Original}
	if ast, ok := bc.checkedASTs[key]; ok {
		return ast, nil
	}

	parsedAST, issues := env.Parse(expr.Original)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	checkedAST, issues := env.Check(parsedAST)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}

	bc.checkedASTs[key] = checkedAST
	return checkedAST, nil
}

// compile parses, type-checks, and compiles a CEL expression. If a checked
// AST was previously stored by parseAndCheck for the same (env, expression),
// the parse+check steps are skipped. Sets expr.Program on success.
func (bc *buildContext) compile(env *cel.Env, expr *krocel.Expression) (*cel.Ast, error) {
	key := checkedASTKey{env: env, expr: expr.Original}
	checkedAST, ok := bc.checkedASTs[key]
	if !ok {
		var err error
		checkedAST, err = bc.parseAndCheck(env, expr)
		if err != nil {
			return nil, err
		}
	}

	program, err := env.Program(checkedAST)
	if err != nil {
		return nil, fmt.Errorf("compile: %w", err)
	}
	expr.Program = program
	return checkedAST, nil
}

// extendWithTypedVar returns a cached environment extending the parent with a
// single typed variable declaration derived from the given schema. Uses
// schemaDeclType for memoized schema→DeclType conversion.
func (bc *buildContext) extendWithTypedVar(parent *cel.Env, varName string, s *spec.Schema) (*cel.Env, error) {
	key := extendedEnvKey{parent: parent, varName: varName}
	if env, ok := bc.extendedEnvs[key]; ok {
		return env, nil
	}

	declType := bc.schemaDeclType(s)
	if declType == nil {
		return nil, fmt.Errorf("failed to build DeclType for schema")
	}

	typeName := krocel.TypeNamePrefix + varName
	declType = declType.MaybeAssignTypeName(typeName)

	provider := krocel.NewDeclTypeProvider(declType)
	provider.SetRecognizeKeywordAsFieldName(true)

	celType := declType.CelType()

	registry := types.NewEmptyRegistry()
	wrappedProvider, err := provider.WithTypeProvider(registry)
	if err != nil {
		return nil, err
	}

	extended, err := parent.Extend(
		cel.Variable(varName, celType),
		cel.CustomTypeProvider(wrappedProvider),
	)
	if err != nil {
		return nil, err
	}

	bc.extendedEnvs[key] = extended
	return extended, nil
}
