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
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/kube-openapi/pkg/validation/spec"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/conversion"
	"github.com/kubernetes-sigs/kro/pkg/graph/fieldpath"
	"github.com/kubernetes-sigs/kro/pkg/graph/schema"
)

// TypeCheckResult is analysis metadata — not a new RGD type.
// Passed alongside LinkedRGD to the compiler.
type TypeCheckResult struct {
	Env               *cel.Env
	TypeProvider      *krocel.DeclTypeProvider
	StatusTypes       map[string]*cel.Type            // instance status field path -> CEL type
	IteratorTypes     map[string]map[string]*cel.Type // resourceID -> iterName -> elemType
	NodeCompileEnvs   map[string]*cel.Env             // resourceID -> env used for template fields
	NodeReadyWhenEnvs map[string]*cel.Env             // resourceID -> env used for readyWhen
	CheckedExprs      map[*LinkedExpr]*cel.Ast
}

type typechecker struct{}

func newTypeChecker() TypeChecker { return &typechecker{} }

// Check type-checks all expressions in the LinkedRGD and returns analysis metadata.
// The LinkedRGD itself is not modified — the TypeCheckResult is passed alongside
// it to the compiler.
func (tc *typechecker) Check(linked *LinkedRGD) (*TypeCheckResult, error) {
	// Collect schemas: collections wrapped as lists
	celSchemas := make(map[string]*spec.Schema)
	for _, n := range linked.Nodes {
		if n.Type == NodeTypeCollection {
			celSchemas[n.ID] = schema.WrapSchemaAsList(n.Schema)
		} else {
			celSchemas[n.ID] = n.Schema
		}
	}

	// SpecSchema is required by the API contract and parse pipeline invariants.
	instanceSchema, err := instanceSchemaWithoutStatusForCEL(linked.Instance.SpecSchema)
	if err != nil {
		return nil, terminalf("typechecker", "spec schema: %v", err)
	}
	celSchemas[SchemaVarName] = instanceSchema

	env, err := krocel.TypedEnvironment(celSchemas)
	if err != nil {
		return nil, terminalf("typechecker", "typed env: %v", err)
	}
	typeProvider := krocel.CreateDeclTypeProvider(celSchemas)

	// Check each node
	iteratorTypes := make(map[string]map[string]*cel.Type)
	nodeCompileEnvs := make(map[string]*cel.Env)
	nodeReadyWhenEnvs := make(map[string]*cel.Env)
	checkedExprs := make(map[*LinkedExpr]*cel.Ast)
	for _, n := range linked.Nodes {
		iterTypes, compileEnv, readyWhenEnv, err := tc.checkNode(n, env, typeProvider, checkedExprs)
		if err != nil {
			return nil, terminal("typechecker", err)
		}
		if len(iterTypes) > 0 {
			iteratorTypes[n.ID] = iterTypes
		}
		nodeCompileEnvs[n.ID] = compileEnv
		nodeReadyWhenEnvs[n.ID] = readyWhenEnv
	}

	// Infer status field CEL types
	statusTypes, err := tc.inferStatus(linked.Instance, env, typeProvider, checkedExprs)
	if err != nil {
		return nil, terminal("typechecker", err)
	}

	return &TypeCheckResult{
		Env:               env,
		TypeProvider:      typeProvider,
		StatusTypes:       statusTypes,
		IteratorTypes:     iteratorTypes,
		NodeCompileEnvs:   nodeCompileEnvs,
		NodeReadyWhenEnvs: nodeReadyWhenEnvs,
		CheckedExprs:      checkedExprs,
	}, nil
}

func (tc *typechecker) checkNode(
	r *LinkedNode,
	env *cel.Env,
	tp *krocel.DeclTypeProvider,
	checkedExprs map[*LinkedExpr]*cel.Ast,
) (map[string]*cel.Type, *cel.Env, *cel.Env, error) {
	// forEach — must return list, infer element type
	var iterTypes map[string]*cel.Type
	if len(r.ForEach) > 0 {
		iterTypes = make(map[string]*cel.Type, len(r.ForEach))
		for _, fe := range r.ForEach {
			checked, err := parseAndCheck(env, fe.Expr.Raw)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("resource %q forEach %q: %w", r.ID, fe.Name, err)
			}
			checkedExprs[fe.Expr] = checked
			elem, err := krocel.ListElementType(checked.OutputType())
			if err != nil {
				return nil, nil, nil, fmt.Errorf("resource %q forEach %q: must return a list, got %q", r.ID, fe.Name, checked.OutputType())
			}
			iterTypes[fe.Name] = elem
		}
	}

	// Extend env with iterator types for template checking
	compileEnv := env
	if len(iterTypes) > 0 {
		opts := make([]cel.EnvOption, 0, len(iterTypes))
		for name, typ := range iterTypes {
			opts = append(opts, cel.Variable(name, typ))
		}
		var err error
		compileEnv, err = env.Extend(opts...)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("resource %q: extend env: %w", r.ID, err)
		}
	}

	// Template fields
	for _, f := range r.Fields {
		expected := expectedType(f, r.Schema, r.ID)
		for _, expr := range f.Exprs {
			checked, err := parseAndCheck(compileEnv, expr.Raw)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("resource %q %q: %w", r.ID, f.Path, err)
			}
			checkedExprs[expr] = checked
			if err := checkType(checked.OutputType(), expected, expr.Raw, r.ID, f.Path, tp); err != nil {
				return nil, nil, nil, err
			}
		}
	}

	// includeWhen -> bool
	for _, expr := range r.IncludeWhen {
		checked, err := checkBool(env, expr, "includeWhen", r.ID)
		if err != nil {
			return nil, nil, nil, err
		}
		checkedExprs[expr] = checked
	}

	// readyWhen -> bool (collections use "each")
	readyEnv := env
	if r.Type == NodeTypeCollection {
		var err error
		readyEnv, err = krocel.TypedEnvironment(map[string]*spec.Schema{EachVarName: r.Schema})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("resource %q: readyWhen env: %w", r.ID, err)
		}
	}
	for _, expr := range r.ReadyWhen {
		checked, err := checkBool(readyEnv, expr, "readyWhen", r.ID)
		if err != nil {
			return nil, nil, nil, err
		}
		checkedExprs[expr] = checked
	}

	return iterTypes, compileEnv, readyEnv, nil
}

func (tc *typechecker) inferStatus(
	inst *LinkedInstance,
	env *cel.Env,
	tp *krocel.DeclTypeProvider,
	checkedExprs map[*LinkedExpr]*cel.Ast,
) (map[string]*cel.Type, error) {
	if len(inst.StatusFields) == 0 {
		return map[string]*cel.Type{}, nil
	}

	typeMap := make(map[string]*cel.Type)
	for _, f := range inst.StatusFields {
		if f.Standalone {
			checked, err := parseAndCheck(env, f.Exprs[0].Raw)
			if err != nil {
				return nil, fmt.Errorf("status %q: %w", f.Path, err)
			}
			checkedExprs[f.Exprs[0]] = checked
			typeMap[f.Path] = checked.OutputType()
		} else {
			for _, expr := range f.Exprs {
				checked, err := parseAndCheck(env, expr.Raw)
				if err != nil {
					return nil, fmt.Errorf("status %q %q: %w", f.Path, expr.Raw, err)
				}
				checkedExprs[expr] = checked
				if err := checkType(checked.OutputType(), cel.StringType, expr.Raw, "status", f.Path, tp); err != nil {
					return nil, err
				}
			}
			typeMap[f.Path] = cel.StringType
		}
	}

	return typeMap, nil
}

func parseAndCheck(env *cel.Env, raw string) (*cel.Ast, error) {
	parsed, iss := env.Parse(raw)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	checked, iss := env.Check(parsed)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	return checked, nil
}

func checkBool(env *cel.Env, expr *LinkedExpr, condType, resourceID string) (*cel.Ast, error) {
	checked, err := parseAndCheck(env, expr.Raw)
	if err != nil {
		return nil, fmt.Errorf("resource %q %s %q: %w", resourceID, condType, expr.Raw, err)
	}
	if !conversion.IsBoolOrOptionalBool(checked.OutputType()) {
		return nil, fmt.Errorf("resource %q %s %q: must return bool, got %q", resourceID, condType, expr.Raw, checked.OutputType())
	}
	return checked, nil
}

func checkType(got, want *cel.Type, expr, resourceID, path string, tp *krocel.DeclTypeProvider) error {
	compatible, compatErr := krocel.AreTypesStructurallyCompatible(got, want, tp)
	if compatible {
		return nil
	}
	if compatErr != nil {
		return fmt.Errorf("type mismatch in resource %q at path %q: expression %q returns type %q but expected %q: %w",
			resourceID, path, expr, got, want, compatErr)
	}
	return fmt.Errorf("type mismatch in resource %q at path %q: expression %q returns type %q but expected %q",
		resourceID, path, expr, got, want)
}

func expectedType(f *LinkedField, rootSchema *spec.Schema, resourceID string) *cel.Type {
	if !f.Standalone {
		return cel.StringType
	}
	if rootSchema == nil {
		return cel.DynType
	}

	segments, err := fieldpath.Parse(f.Path)
	if err != nil {
		return cel.DynType
	}

	schemaAtPath, typeName, err := resolveSchemaAndTypeName(segments, rootSchema, resourceID)
	if err != nil {
		return cel.DynType
	}

	return celTypeFromSchema(*schemaAtPath, typeName)
}

func celTypeFromSchema(s spec.Schema, typeName string) *cel.Type {
	declType := krocel.SchemaDeclTypeWithMetadata(&openapi.Schema{Schema: &s}, false)
	if declType == nil {
		return cel.DynType
	}
	declType = declType.MaybeAssignTypeName(typeName)
	return declType.CelType()
}

func resolveSchemaAndTypeName(segments []fieldpath.Segment, rootSchema *spec.Schema, resourceID string) (*spec.Schema, string, error) {
	current := rootSchema
	typeName := krocel.TypeNamePrefix + resourceID

	for _, seg := range segments {
		if seg.Name != "" {
			typeName = typeName + "." + seg.Name
			current = lookupSchemaAtField(current, seg.Name)
			if current == nil {
				return nil, "", fmt.Errorf("field %q not found in schema", seg.Name)
			}
		}

		if seg.Index != -1 {
			if current == nil || current.Items == nil || current.Items.Schema == nil {
				return nil, "", fmt.Errorf("field is not an array")
			}
			current = current.Items.Schema
			typeName = typeName + ".@idx"
		}
	}

	return current, typeName, nil
}

func lookupSchemaAtField(schema *spec.Schema, field string) *spec.Schema {
	if prop, ok := schema.Properties[field]; ok {
		return &prop
	}

	if schema.AdditionalProperties != nil {
		if schema.AdditionalProperties.Schema != nil {
			return schema.AdditionalProperties.Schema
		}
		if schema.AdditionalProperties.Allows {
			return &spec.Schema{}
		}
	}

	if schema.Items != nil && schema.Items.Schema != nil {
		return lookupSchemaAtField(schema.Items.Schema, field)
	}

	return nil
}

func instanceSchemaWithoutStatusForCEL(specSchema *extv1.JSONSchemaProps) (*spec.Schema, error) {
	openAPI := &extv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]extv1.JSONSchemaProps{
			"apiVersion": {Type: "string"},
			"kind":       {Type: "string"},
			"spec":       *specSchema.DeepCopy(),
		},
	}
	s, err := schema.ConvertJSONSchemaPropsToSpecSchema(openAPI)
	if err != nil {
		return nil, err
	}
	if s.Properties == nil {
		s.Properties = make(map[string]spec.Schema)
	}
	s.Properties["metadata"] = schema.ObjectMetaSchema
	return s, nil
}
