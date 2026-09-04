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

package cel

import (
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	apiservercel "k8s.io/apiserver/pkg/cel"
	"k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// SchemaDeclType converts an OpenAPI schema into a CEL DeclType. Returns nil
// for a nil schema. This is the pointer-cache-free core shared by the
// per-build memoizing wrappers in pkg/graph and pkg/graphengine/compiler.
func SchemaDeclType(s *spec.Schema) *apiservercel.DeclType {
	if s == nil {
		return nil
	}
	return SchemaDeclTypeWithMetadata(&openapi.Schema{Schema: s}, false)
}

// ParseAndCheck parses and type-checks expr against env, returning the checked
// AST. It performs no compilation and no caching; callers layer their own
// per-build memoization on top.
func ParseAndCheck(env *cel.Env, expr string) (*cel.Ast, error) {
	parsed, issues := env.Parse(expr)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	checked, issues := env.Check(parsed)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	return checked, nil
}

// ExtendWithTypedVar extends parent with a single typed variable declaration
// derived from declType. The caller is responsible for resolving declType
// (and handling a nil result) plus any environment caching. declType must be
// non-nil.
func ExtendWithTypedVar(parent *cel.Env, varName string, declType *apiservercel.DeclType) (*cel.Env, error) {
	typeName := TypeNamePrefix + varName
	declType = declType.MaybeAssignTypeName(typeName)

	provider := NewDeclTypeProvider(declType)
	provider.SetRecognizeKeywordAsFieldName(true)

	celType := declType.CelType()

	registry := types.NewEmptyRegistry()
	wrappedProvider, err := provider.WithTypeProvider(registry)
	if err != nil {
		return nil, err
	}

	return parent.Extend(
		cel.Variable(varName, celType),
		cel.CustomTypeProvider(wrappedProvider),
	)
}
