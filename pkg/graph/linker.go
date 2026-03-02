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
	"slices"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/ast"
	"github.com/kubernetes-sigs/kro/pkg/graph/dag"
)

// LinkedExpr is a parsed expression annotated with referenced identifiers.
type LinkedExpr struct {
	Raw        string
	References []string
}

// LinkedField is a field with linked expressions and field-kind classification.
type LinkedField struct {
	Path       string
	Standalone bool
	Kind       FieldKind
	Exprs      []*LinkedExpr
}

// LinkedForEach is a linked iterator expression for collection nodes.
type LinkedForEach struct {
	Name string
	Expr *LinkedExpr
}

// LinkedNode is a resolved node annotated with expression links and dependencies.
type LinkedNode struct {
	NodeIdentity
	Dependencies []string
	Fields       []*LinkedField
	ReadyWhen    []*LinkedExpr
	IncludeWhen  []*LinkedExpr
	ForEach      []*LinkedForEach
}

// LinkedInstance is the linked representation of instance status fields.
type LinkedInstance struct {
	InstanceMeta
	StatusFields []*LinkedField
}

// LinkedRGD is the linker-stage output including dependency graph order.
type LinkedRGD struct {
	Instance         *LinkedInstance
	Nodes            []*LinkedNode
	DAG              *dag.DirectedAcyclicGraph[string]
	TopologicalOrder []string
}

type linker struct{}

func newLinker() Linker { return &linker{} }

type scopeRule struct {
	condType    string
	raws        []*RawExpr
	allowedRefs []string
}

// Link takes ResolvedRGD and returns LinkedRGD with dependency DAG,
// classified variables, and validated scope rules.
func (l *linker) Link(resolved *ResolvedRGD) (*LinkedRGD, error) {
	// Collect identifiers
	nodeIDs := make([]string, 0, len(resolved.Nodes))
	for _, n := range resolved.Nodes {
		nodeIDs = append(nodeIDs, n.ID)
	}
	allIDs := append(slices.Clone(nodeIDs), SchemaVarName, EachVarName)

	env, err := krocel.DefaultEnvironment(krocel.WithResourceIDs(allIDs))
	if err != nil {
		return nil, terminalf("linker", "inspector env: %v", err)
	}
	inspector := ast.NewInspectorWithEnv(env, allIDs)

	// Build DAG
	d := dag.NewDirectedAcyclicGraph[string]()
	for _, n := range resolved.Nodes {
		if err := d.AddVertex(n.ID, n.Index); err != nil {
			return nil, terminalf("linker", "vertex %q: %v", n.ID, err)
		}
	}

	// Link each node
	linkedNodes := make([]*LinkedNode, 0, len(resolved.Nodes))
	for _, n := range resolved.Nodes {
		ln, err := l.linkNode(n, inspector)
		if err != nil {
			return nil, terminal("linker", err)
		}
		if err := d.AddDependencies(ln.ID, ln.Dependencies); err != nil {
			return nil, terminal("linker", err)
		}
		linkedNodes = append(linkedNodes, ln)
	}

	order, err := d.TopologicalSort()
	if err != nil {
		return nil, terminal("linker", err)
	}

	// Link instance status fields
	linkedInstance, err := l.linkInstance(resolved.Instance, inspector, nodeIDs)
	if err != nil {
		return nil, terminal("linker", err)
	}

	return &LinkedRGD{
		Instance:         linkedInstance,
		Nodes:            linkedNodes,
		DAG:              d,
		TopologicalOrder: order,
	}, nil
}

func (l *linker) linkNode(r *ResolvedNode, inspector *ast.Inspector) (*LinkedNode, error) {
	iterNames := iteratorNames(r)
	allDeps := make([]string, 0)
	seenDeps := make(map[string]struct{})
	addDeps := func(deps []string) {
		for _, dep := range deps {
			if _, ok := seenDeps[dep]; ok {
				continue
			}
			seenDeps[dep] = struct{}{}
			allDeps = append(allDeps, dep)
		}
	}

	// Link template fields -> LinkedField
	linkedFields := make([]*LinkedField, 0, len(r.Fields))
	for _, f := range r.Fields {
		lf, deps, err := l.linkField(f, inspector, iterNames)
		if err != nil {
			return nil, fmt.Errorf("resource %q path %q: %w", r.ID, f.Path, err)
		}
		linkedFields = append(linkedFields, lf)
		addDeps(deps)
	}

	// Validate forEach iterators are used in identity fields
	if len(iterNames) > 0 {
		if err := l.validateIteratorUsage(r, linkedFields, iterNames); err != nil {
			return nil, err
		}
	}

	// Link forEach expressions
	linkedForEach := make([]*LinkedForEach, 0, len(r.ForEach))
	for _, fe := range r.ForEach {
		deps, iterRefs, le, err := l.linkExpr(inspector, fe.Expr, iterNames)
		if err != nil {
			return nil, fmt.Errorf("resource %q forEach %q: %w", r.ID, fe.Name, err)
		}
		if len(iterRefs) > 0 {
			return nil, fmt.Errorf("resource %q: forEach %q cannot reference other iterators %v", r.ID, fe.Name, iterRefs)
		}
		linkedForEach = append(linkedForEach, &LinkedForEach{Name: fe.Name, Expr: le})
		addDeps(deps)
	}

	allowedVar := r.ID
	if r.Type == NodeTypeCollection {
		allowedVar = EachVarName
	}

	// Link + scope-validate includeWhen/readyWhen.
	linkedInclude, err := l.linkAndValidateScopeRule(r.ID, inspector, iterNames, scopeRule{
		condType:    "includeWhen",
		raws:        r.IncludeWhen,
		allowedRefs: []string{SchemaVarName},
	})
	if err != nil {
		return nil, err
	}
	linkedReady, err := l.linkAndValidateScopeRule(r.ID, inspector, iterNames, scopeRule{
		condType:    "readyWhen",
		raws:        r.ReadyWhen,
		allowedRefs: []string{allowedVar},
	})
	if err != nil {
		return nil, err
	}

	return &LinkedNode{
		NodeIdentity: IdentityFrom(r),
		Dependencies: allDeps,
		Fields:       linkedFields,
		ReadyWhen:    linkedReady,
		IncludeWhen:  linkedInclude,
		ForEach:      linkedForEach,
	}, nil
}

func (l *linker) linkInstance(inst *ParsedInstance, inspector *ast.Inspector, nodeIDs []string) (*LinkedInstance, error) {
	linkedStatus := make([]*LinkedField, 0, len(inst.StatusFields))
	for _, f := range inst.StatusFields {
		linkedExprs := make([]*LinkedExpr, 0, len(f.Exprs))
		fieldHasResourceReference := false
		for _, expr := range f.Exprs {
			result, err := inspector.Inspect(expr.Raw)
			if err != nil {
				return nil, fmt.Errorf("status %q: %w", f.Path, err)
			}
			le := &LinkedExpr{Raw: expr.Raw}
			for _, dep := range result.ResourceDependencies {
				if dep.ID == SchemaVarName {
					return nil, fmt.Errorf("status %q: cannot reference schema", f.Path)
				}
				if !slices.Contains(nodeIDs, dep.ID) {
					return nil, fmt.Errorf("status %q: unknown resource %q", f.Path, dep.ID)
				}
				if !slices.Contains(le.References, dep.ID) {
					le.References = append(le.References, dep.ID)
				}
				fieldHasResourceReference = true
			}
			if len(result.UnknownResources) > 0 {
				unknownIDs := make([]string, 0, len(result.UnknownResources))
				for _, unk := range result.UnknownResources {
					unknownIDs = append(unknownIDs, unk.ID)
				}
				return nil, fmt.Errorf("status %q: references unknown identifiers: %v", f.Path, unknownIDs)
			}
			linkedExprs = append(linkedExprs, le)
		}
		if !fieldHasResourceReference {
			return nil, fmt.Errorf("status %q: must reference at least one resource", f.Path)
		}
		linkedStatus = append(linkedStatus, &LinkedField{
			Path:       f.Path,
			Standalone: f.Standalone,
			Kind:       FieldDynamic,
			Exprs:      linkedExprs,
		})
	}
	return &LinkedInstance{
		InstanceMeta: inst.InstanceMeta,
		StatusFields: linkedStatus,
	}, nil
}

// linkField converts a RawField -> LinkedField and returns resource deps.
// Field kind is classified here: FieldIteration if any expression uses
// iterators, FieldDynamic if any expression depends on a resource node,
// FieldStatic otherwise (schema-only or literal).
func (l *linker) linkField(f *RawField, inspector *ast.Inspector, iterNames []string) (*LinkedField, []string, error) {
	linkedExprs := make([]*LinkedExpr, 0, len(f.Exprs))
	var allDeps []string
	usesIterators := false

	for _, expr := range f.Exprs {
		deps, iterRefs, le, err := l.linkExpr(inspector, expr, iterNames)
		if err != nil {
			return nil, nil, err
		}
		linkedExprs = append(linkedExprs, le)
		allDeps = append(allDeps, deps...)
		if len(iterRefs) > 0 {
			usesIterators = true
		}
	}

	var kind FieldKind
	switch {
	case usesIterators:
		kind = FieldIteration
	case len(allDeps) > 0:
		kind = FieldDynamic
	default:
		kind = FieldStatic
	}

	return &LinkedField{
		Path:       f.Path,
		Standalone: f.Standalone,
		Kind:       kind,
		Exprs:      linkedExprs,
	}, allDeps, nil
}

// linkExpr inspects a raw expression. Returns (resourceDeps, iteratorRefs, linkedExpr, error).
func (l *linker) linkExpr(inspector *ast.Inspector, raw *RawExpr, iterNames []string) ([]string, []string, *LinkedExpr, error) {
	result, err := inspector.Inspect(raw.Raw)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("inspect %q: %w", raw.Raw, err)
	}

	le := &LinkedExpr{Raw: raw.Raw}
	var resourceDeps, iterRefs []string

	for _, dep := range result.ResourceDependencies {
		if !slices.Contains(le.References, dep.ID) {
			le.References = append(le.References, dep.ID)
		}
		if dep.ID == SchemaVarName {
			continue
		}
		if !slices.Contains(resourceDeps, dep.ID) {
			resourceDeps = append(resourceDeps, dep.ID)
		}
	}

	for _, unk := range result.UnknownResources {
		if slices.Contains(iterNames, unk.ID) {
			if !slices.Contains(iterRefs, unk.ID) {
				iterRefs = append(iterRefs, unk.ID)
			}
			if !slices.Contains(le.References, unk.ID) {
				le.References = append(le.References, unk.ID)
			}
		} else {
			return nil, nil, nil, fmt.Errorf("references unknown identifiers: [%s]", unk.ID)
		}
	}

	if len(result.UnknownFunctions) > 0 {
		return nil, nil, nil, fmt.Errorf("unknown functions: %v", result.UnknownFunctions)
	}

	return resourceDeps, iterRefs, le, nil
}

func (l *linker) linkConditions(inspector *ast.Inspector, raws []*RawExpr, iterNames []string) ([]*LinkedExpr, error) {
	out := make([]*LinkedExpr, 0, len(raws))
	for _, raw := range raws {
		_, _, le, err := l.linkExpr(inspector, raw, iterNames)
		if err != nil {
			return nil, err
		}
		out = append(out, le)
	}
	return out, nil
}

func (l *linker) linkAndValidateScopeRule(resourceID string, inspector *ast.Inspector, iterNames []string, rule scopeRule) ([]*LinkedExpr, error) {
	linked, err := l.linkConditions(inspector, rule.raws, iterNames)
	if err != nil {
		return nil, fmt.Errorf("resource %q %s: %w", resourceID, rule.condType, err)
	}
	for _, le := range linked {
		if err := validateScope(le, rule.allowedRefs, rule.condType, resourceID); err != nil {
			return nil, err
		}
	}
	return linked, nil
}

func (l *linker) validateIteratorUsage(r *ResolvedNode, fields []*LinkedField, iterNames []string) error {
	used := map[string]bool{}
	for _, f := range fields {
		isIdentity := f.Path == MetadataNamePath ||
			(f.Path == MetadataNamespacePath && r.Namespaced)
		if !isIdentity {
			continue
		}
		for _, expr := range f.Exprs {
			for _, ref := range expr.References {
				if slices.Contains(iterNames, ref) {
					used[ref] = true
				}
			}
		}
	}
	var missing []string
	for _, name := range iterNames {
		if !used[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("resource %q: all forEach dimensions must be used to produce a unique resource identity, missing: %v", r.ID, missing)
	}
	return nil
}

func validateScope(expr *LinkedExpr, allowed []string, condType, resourceID string) error {
	var unknown []string
	for _, ref := range expr.References {
		if !slices.Contains(allowed, ref) {
			unknown = append(unknown, ref)
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("resource %q %s: references unknown identifiers: %v", resourceID, condType, unknown)
	}
	return nil
}

func iteratorNames(r *ResolvedNode) []string {
	out := make([]string, len(r.ForEach))
	for i, fe := range r.ForEach {
		out[i] = fe.Name
	}
	return out
}
