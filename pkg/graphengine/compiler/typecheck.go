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

package compiler

import (
	"fmt"
	"slices"
	"strings"

	"github.com/google/cel-go/cel"
	apiservercel "k8s.io/apiserver/pkg/cel"
	"k8s.io/kube-openapi/pkg/validation/spec"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph/fieldpath"
	"github.com/kubernetes-sigs/kro/pkg/graph/schema"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

// buildContext memoizes per-build CEL artifacts so a single Compile() call
// doesn't redo work for the same (env, expression) or (env, var) pair. It
// is never stored beyond the Compile invocation that owns it.
type buildContext struct {
	env          *cel.Env
	typeProvider *krocel.DeclTypeProvider
	schemaCache  *schema.Cache
	costLimit    uint64

	declTypes    map[*spec.Schema]*apiservercel.DeclType
	checkedASTs  map[checkedASTKey]*cel.Ast
	extendedEnvs map[extendedEnvKey]*cel.Env
}

type checkedASTKey struct {
	env  *cel.Env
	expr string
}

type extendedEnvKey struct {
	parent *cel.Env
	// fingerprint encodes the iterator variable declarations contributed by
	// this extension. We use a string fingerprint so a map key can describe
	// a multi-variable extension without dragging in unbounded shape.
	fingerprint string
}

// newBuildContext constructs a fresh buildContext bound to a typed env and
// its provider.
func newBuildContext(env *cel.Env, provider *krocel.DeclTypeProvider, cache *schema.Cache, costLimit uint64) *buildContext {
	return &buildContext{
		env:          env,
		typeProvider: provider,
		schemaCache:  cache,
		costLimit:    costLimit,
		declTypes:    make(map[*spec.Schema]*apiservercel.DeclType),
		checkedASTs:  make(map[checkedASTKey]*cel.Ast),
		extendedEnvs: make(map[extendedEnvKey]*cel.Env),
	}
}

// schemaDeclType returns a DeclType for s, memoized by pointer identity.
func (bc *buildContext) schemaDeclType(s *spec.Schema) *apiservercel.DeclType {
	if s == nil {
		return nil
	}
	if dt, ok := bc.declTypes[s]; ok {
		return dt
	}
	dt := krocel.SchemaDeclType(s)
	bc.declTypes[s] = dt
	return dt
}

// parseAndCheck parses + type-checks expr in env, caching the resulting
// checked AST so a later compile() call can skip the work.
func (bc *buildContext) parseAndCheck(env *cel.Env, expr *krocel.Expression) (*cel.Ast, error) {
	key := checkedASTKey{env: env, expr: expr.Original}
	if ast, ok := bc.checkedASTs[key]; ok {
		return ast, nil
	}
	checked, err := krocel.ParseAndCheck(env, expr.Original)
	if err != nil {
		return nil, err
	}
	bc.checkedASTs[key] = checked
	return checked, nil
}

// compile parses, type-checks, and compiles expr, populating expr.Program.
// Returns the checked AST so callers can inspect the output type.
func (bc *buildContext) compile(env *cel.Env, expr *krocel.Expression) (*cel.Ast, error) {
	checked, err := bc.parseAndCheck(env, expr)
	if err != nil {
		return nil, err
	}
	prog, err := env.Program(checked, krocel.ProgramOptions(bc.costLimit)...)
	if err != nil {
		return nil, fmt.Errorf("program: %w", err)
	}
	expr.Program = prog
	return checked, nil
}

// extendWithIterators returns a cached env that augments parent with the
// given iterator variable declarations. Iterator types are concrete CEL
// types (already inferred from forEach expression output).
func (bc *buildContext) extendWithIterators(parent *cel.Env, iterators map[string]*cel.Type) (*cel.Env, error) {
	if len(iterators) == 0 {
		return parent, nil
	}
	fp := iteratorFingerprint(iterators)
	key := extendedEnvKey{parent: parent, fingerprint: fp}
	if env, ok := bc.extendedEnvs[key]; ok {
		return env, nil
	}
	opts := make([]cel.EnvOption, 0, len(iterators))
	for name, t := range iterators {
		opts = append(opts, cel.Variable(name, t))
	}
	extended, err := parent.Extend(opts...)
	if err != nil {
		return nil, err
	}
	bc.extendedEnvs[key] = extended
	return extended, nil
}

// extendWithTypedVar returns a cached env that augments parent with a single
// typed variable declaration derived from schema. Binds the collection
// iterator variable (`each`) to the node's element schema when type-checking
// collection readyWhen.
func (bc *buildContext) extendWithTypedVar(parent *cel.Env, varName string, s *spec.Schema) (*cel.Env, error) {
	if s == nil {
		return parent, nil
	}
	fp := "typedvar\x00" + varName + "\x00" + fmt.Sprintf("%p", s)
	key := extendedEnvKey{parent: parent, fingerprint: fp}
	if env, ok := bc.extendedEnvs[key]; ok {
		return env, nil
	}

	declType := bc.schemaDeclType(s)
	if declType == nil {
		return nil, fmt.Errorf("build DeclType for schema: unsupported or empty schema")
	}

	extended, err := krocel.ExtendWithTypedVar(parent, varName, declType)
	if err != nil {
		return nil, err
	}
	bc.extendedEnvs[key] = extended
	return extended, nil
}

// iteratorFingerprint produces a deterministic key for an iterator type
// map so the cache lookup is order-insensitive.
func iteratorFingerprint(iterators map[string]*cel.Type) string {
	keys := make([]string, 0, len(iterators))
	for name := range iterators {
		keys = append(keys, name)
	}
	slices.Sort(keys)

	parts := make([]string, 0, len(iterators)*2)
	for _, k := range keys {
		parts = append(parts, k, iterators[k].String())
	}
	return strings.Join(parts, "\x00")
}

// expectedTypeForField returns the CEL type expected at descriptor.Path
// within rootSchema. Returns cel.DynType when the path can't be resolved or
// rootSchema is nil — callers get permissive type-checking in those cases.
func expectedTypeForField(bc *buildContext, descriptor *variable.FieldDescriptor, rootSchema *spec.Schema, nodeID string) *cel.Type {
	if rootSchema == nil {
		return cel.DynType
	}
	segments, err := fieldpath.Parse(descriptor.Path)
	if err != nil {
		return cel.DynType
	}
	s, typeName, err := resolveSchemaAndTypeName(bc.schemaCache, segments, rootSchema, nodeID)
	if err != nil {
		return cel.DynType
	}
	return celTypeFromSchema(bc, s, typeName)
}

// resolveSchemaAndTypeName walks segments through rootSchema and produces
// (leafSchema, qualifiedTypeName). Index segments dereference array Items.
func resolveSchemaAndTypeName(c *schema.Cache, segments []fieldpath.Segment, rootSchema *spec.Schema, nodeID string) (*spec.Schema, string, error) {
	var sb strings.Builder
	sb.WriteString(krocel.TypeNamePrefix)
	sb.WriteString(nodeID)

	current := rootSchema
	for _, seg := range segments {
		if seg.Name != "" {
			sb.WriteByte('.')
			sb.WriteString(seg.Name)
			current = lookupSchemaAtField(c, current, seg.Name)
			if current == nil {
				return nil, "", fmt.Errorf("field %q not found", seg.Name)
			}
		}
		if seg.Index != -1 {
			if current.Items != nil && current.Items.Schema != nil {
				current = current.Items.Schema
				sb.WriteString(".@idx")
			} else {
				return nil, "", fmt.Errorf("index on non-array at %q", seg.Name)
			}
		}
	}
	return current, sb.String(), nil
}

// lookupSchemaAtField resolves a single field name within a schema using
// the cache for pointer-stable lookups. Walks into Items for array shapes.
func lookupSchemaAtField(c *schema.Cache, s *spec.Schema, field string) *spec.Schema {
	if s == nil || field == "" {
		return s
	}
	if r := c.LookupField(s, field); r != nil {
		return r
	}
	if r := c.LookupAdditionalProperties(s); r != nil {
		return r
	}
	if s.Items != nil && s.Items.Schema != nil {
		return lookupSchemaAtField(c, s.Items.Schema, field)
	}
	return nil
}

// celTypeFromSchema converts a (schema, qualifiedName) pair into a CEL type,
// preferring the provider's registered type for a hash-cached lookup before
// falling back to ad-hoc schema conversion.
func celTypeFromSchema(bc *buildContext, s *spec.Schema, typeName string) *cel.Type {
	if bc.typeProvider != nil {
		if dt, ok := bc.typeProvider.FindDeclType(typeName); ok {
			return dt.CelType()
		}
	}
	dt := bc.schemaDeclType(s)
	if dt == nil {
		return cel.DynType
	}
	dt = dt.MaybeAssignTypeName(typeName)
	return dt.CelType()
}

// validateAndCompileNode runs the type-checking + compilation pass for a
// single node. The passed nodeSchema describes the publication shape for
// Template nodes (i.e. the actual payload). For Ref/Def the payload
// doesn't match nodeSchema; callers pass nil to fall back to dyn.
func validateAndCompileNode(bc *buildContext, n *Node, payloadSchema *spec.Schema, elementSchema *spec.Schema) error {
	iteratorTypes, err := validateAndCompileForEach(bc, n)
	if err != nil {
		return err
	}

	compileEnv, err := bc.extendWithIterators(bc.env, iteratorTypes)
	if err != nil {
		return fmt.Errorf("extend env with iterators: %w", err)
	}

	for _, v := range n.Variables {
		expected := expectedTypeForField(bc, &v.FieldDescriptor, payloadSchema, n.ID)
		checked, err := bc.compile(compileEnv, v.Expression)
		if err != nil {
			return fmt.Errorf("variable at %q (%q): %w", v.Path, v.Expression.UserExpression(), err)
		}
		if err := validateExpressionType(checked.OutputType(), expected, v.Expression.UserExpression(), n.ID, v.Path, bc.typeProvider); err != nil {
			return err
		}
	}

	// includeWhen evaluates at the node level (no iterator scope), against the
	// typed env.
	if err := validateAndCompileConditions(bc, bc.env, n.IncludeWhen, n.ID, "includeWhen"); err != nil {
		return err
	}
	// readyWhen for a collection is evaluated per-element with `each` bound to
	// the node's element schema; type-check it against an env extended with
	// that typed var. Non-collection readyWhen keeps compiling against bc.env.
	readyEnv := bc.env
	if n.IsCollection() {
		if elementSchema == nil {
			// A dynamic-GVK collection (e.g. a Ref-with-selector whose GVK is
			// a CEL expression) has no compile-time element schema. The
			// runtime still binds `each` per element, so declare it as dyn —
			// mirroring the forEach dyn-iterator path — rather than leaving it
			// undeclared, which would make a readyWhen referencing `each` fail
			// to compile.
			readyEnv, err = bc.extendWithIterators(bc.env, map[string]*cel.Type{EachVarName: cel.DynType})
			if err != nil {
				return fmt.Errorf("extend env with dyn %q for readyWhen: %w", EachVarName, err)
			}
		} else {
			readyEnv, err = bc.extendWithTypedVar(bc.env, EachVarName, elementSchema)
			if err != nil {
				return fmt.Errorf("extend env with %q for readyWhen: %w", EachVarName, err)
			}
		}
	}
	if err := validateAndCompileConditions(bc, readyEnv, n.ReadyWhen, n.ID, "readyWhen"); err != nil {
		return err
	}
	return nil
}

// validateAndCompileConditions compiles each bool-returning condition
// expression against bc.env. Verifies the output is assignable to bool —
// concretely-typed bools pass; dyn passes (def-sourced expressions can't
// be statically narrowed to bool); anything else (string, int, list, etc.)
// is almost always a user mistake.
func validateAndCompileConditions(bc *buildContext, env *cel.Env, exprs []*krocel.Expression, nodeID, kind string) error {
	for i, expr := range exprs {
		checked, err := bc.compile(env, expr)
		if err != nil {
			return fmt.Errorf("node %q: %s[%d] (%q): %w", nodeID, kind, i, expr.UserExpression(), err)
		}
		out := checked.OutputType()
		// Accept bool or dyn. dyn covers def-sourced expressions (the static
		// type can't be narrowed). An optional<bool> is rejected: an empty
		// optional (optional.none()) has no boolean value, which becomes a
		// runtime error where a condition must decide include/ready. Reject it
		// at compile time and tell the author to collapse it to a concrete
		// bool. Anything else (string, int, list, struct) is a user mistake.
		if isOptionalBool(out) {
			return fmt.Errorf("node %q: %s[%d] (%q) returns optional<bool>, which becomes a runtime error when empty; "+
				"make it a concrete bool (e.g. append .orValue(false))",
				nodeID, kind, i, expr.UserExpression())
		}
		if !cel.BoolType.IsAssignableType(out) && out != cel.DynType {
			return fmt.Errorf("node %q: %s[%d] (%q) must return bool, got %s",
				nodeID, kind, i, expr.UserExpression(), out.String())
		}
	}
	return nil
}

// isOptionalBool reports whether t is optional<bool> (as opposed to a plain
// bool or dyn). Used to reject optional-typed condition expressions, whose
// empty case has no boolean value.
func isOptionalBool(t *cel.Type) bool {
	if cel.BoolType.IsAssignableType(t) {
		return false // a plain bool, not an optional
	}
	return cel.OptionalType(cel.BoolType).IsAssignableType(t)
}

// validateAndCompileForEach compiles each forEach axis, requires it to
// return a list, and reports the element type for the iterator variable.
func validateAndCompileForEach(bc *buildContext, n *Node) (map[string]*cel.Type, error) {
	if len(n.ForEach) == 0 {
		return nil, nil
	}
	out := make(map[string]*cel.Type, len(n.ForEach))
	for i := range n.ForEach {
		iter := &n.ForEach[i]
		checked, err := bc.compile(bc.env, iter.Expression)
		if err != nil {
			return nil, fmt.Errorf("forEach %q (%q): %w", iter.Name, iter.Expression.UserExpression(), err)
		}
		// If the source is dyn (e.g. coming from a Def node), the element
		// type cannot be inferred — bind the iterator as dyn and defer the
		// list-shape check to runtime.
		if checked.OutputType() == cel.DynType {
			out[iter.Name] = cel.DynType
			continue
		}
		elem, err := krocel.ListElementType(checked.OutputType())
		if err != nil {
			return nil, fmt.Errorf("forEach %q must return a list, got %q: %w", iter.Name, checked.OutputType().String(), err)
		}
		out[iter.Name] = elem
	}
	return out, nil
}

// validateExpressionType requires output to be assignable to expected,
// either nominally or structurally (duck-typed via the type provider).
func validateExpressionType(output, expected *cel.Type, exprDisplay, nodeID, path string, provider *krocel.DeclTypeProvider) error {
	if expected.IsAssignableType(output) {
		return nil
	}
	ok, structErr := krocel.AreTypesStructurallyCompatible(output, expected, provider)
	if ok {
		return nil
	}
	if structErr != nil {
		return fmt.Errorf(
			"type mismatch in node %q at path %q: expression %q returns %q but expected %q: %w",
			nodeID, path, exprDisplay, output.String(), expected.String(), structErr,
		)
	}
	return fmt.Errorf(
		"type mismatch in node %q at path %q: expression %q returns %q but expected %q",
		nodeID, path, exprDisplay, output.String(), expected.String(),
	)
}
