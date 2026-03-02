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

	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	graphparser "github.com/kubernetes-sigs/kro/pkg/graph/parser"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
	"github.com/kubernetes-sigs/kro/pkg/simpleschema"
)

// RawExpr stores the unlinked CEL source string for an expression.
type RawExpr struct{ Raw string }

// RawField represents one parsed template/status field before linking.
type RawField struct {
	Path       string
	Standalone bool
	Exprs      []*RawExpr
}

// RawForEach represents one collection iterator before linking.
type RawForEach struct {
	Name string
	Expr *RawExpr
}

// ParsedNode is a normalized resource node after parse stage.
type ParsedNode struct {
	NodeMeta
	HasTemplate    bool
	HasExternalRef bool
	Fields         []*RawField
	ReadyWhen      []*RawExpr
	IncludeWhen    []*RawExpr
	ForEach        []*RawForEach
}

// ParsedInstance is normalized instance schema/status data after parse stage.
type ParsedInstance struct {
	InstanceMeta
	StatusFields []*RawField
}

// ParsedRGD is the parse-stage output consumed by validator/resolver.
type ParsedRGD struct {
	Instance *ParsedInstance
	Nodes    []*ParsedNode
}

type parser struct{}

func newParser() Parser { return &parser{} }

func (p *parser) Parse(rgd *v1alpha1.ResourceGraphDefinition) (*ParsedRGD, error) {
	instance, err := p.parseInstance(rgd.Spec.Schema)
	if err != nil {
		return nil, terminal("parser", fmt.Errorf("instance: %w", err))
	}

	nodes := make([]*ParsedNode, 0, len(rgd.Spec.Resources))
	for i, r := range rgd.Spec.Resources {
		node, err := p.parseNode(r, i)
		if err != nil {
			return nil, terminal("parser", fmt.Errorf("resource %q: %w", r.ID, err))
		}
		nodes = append(nodes, node)
	}

	return &ParsedRGD{Instance: instance, Nodes: nodes}, nil
}

func (p *parser) parseInstance(s *v1alpha1.Schema) (*ParsedInstance, error) {
	specMap := map[string]interface{}{}
	if len(s.Spec.Raw) > 0 {
		if err := yaml.UnmarshalStrict(s.Spec.Raw, &specMap); err != nil {
			return nil, fmt.Errorf("unmarshal spec: %w", err)
		}
	}

	customTypes := map[string]interface{}{}
	if len(s.Types.Raw) > 0 {
		if err := yaml.UnmarshalStrict(s.Types.Raw, &customTypes); err != nil {
			return nil, fmt.Errorf("unmarshal types: %w", err)
		}
	}

	specSchema, err := simpleschema.ToOpenAPISpec(specMap, customTypes)
	if err != nil {
		return nil, fmt.Errorf("failed to build OpenAPI schema for instance: %w", err)
	}

	statusMap := map[string]interface{}{}
	if len(s.Status.Raw) > 0 {
		if err := yaml.UnmarshalStrict(s.Status.Raw, &statusMap); err != nil {
			return nil, fmt.Errorf("unmarshal status: %w", err)
		}
	}

	statusDescriptors, plainStatusFields, err := graphparser.ParseSchemalessResource(statusMap)
	if err != nil {
		return nil, fmt.Errorf("status expressions: %w", err)
	}
	if len(plainStatusFields) > 0 {
		return nil, fmt.Errorf("status fields without expressions are not supported")
	}

	return &ParsedInstance{
		InstanceMeta: InstanceMeta{
			Group:          s.Group,
			APIVersion:     s.APIVersion,
			Kind:           s.Kind,
			SpecSchema:     specSchema,
			CustomTypes:    customTypes,
			StatusTemplate: statusMap,
			PrinterColumns: s.AdditionalPrinterColumns,
			Metadata:       s.Metadata,
		},
		StatusFields: rawFieldsFrom(statusDescriptors),
	}, nil
}

func (p *parser) parseNode(r *v1alpha1.Resource, index int) (*ParsedNode, error) {
	var tmpl map[string]interface{}
	var extRef *v1alpha1.ExternalRef

	hasTemplate := len(r.Template.Raw) > 0
	hasExternalRef := r.ExternalRef != nil
	if hasExternalRef {
		extRef = r.ExternalRef
	}

	if hasTemplate {
		tmpl = map[string]interface{}{}
		if err := yaml.UnmarshalStrict(r.Template.Raw, &tmpl); err != nil {
			return nil, fmt.Errorf("unmarshal: %w", err)
		}
	} else if hasExternalRef {
		tmpl = externalRefToTemplate(extRef)
	}

	descriptors, _, err := graphparser.ParseSchemalessResource(tmpl)
	if err != nil {
		return nil, fmt.Errorf("extract expressions: %w", err)
	}

	readyWhen, err := parseConditions(r.ReadyWhen)
	if err != nil {
		return nil, fmt.Errorf("failed to parse readyWhen expressions: %w", err)
	}

	includeWhen, err := parseConditions(r.IncludeWhen)
	if err != nil {
		return nil, fmt.Errorf("failed to parse includeWhen expressions: %w", err)
	}

	forEach, err := parseForEach(r.ForEach)
	if err != nil {
		return nil, fmt.Errorf("forEach: %w", err)
	}

	typ := NodeTypeScalar
	if extRef != nil {
		typ = NodeTypeExternal
	} else if len(forEach) > 0 {
		typ = NodeTypeCollection
	}

	return &ParsedNode{
		NodeMeta: NodeMeta{
			ID:          r.ID,
			Index:       index,
			Type:        typ,
			Template:    tmpl,
			ExternalRef: extRef,
		},
		HasTemplate:    hasTemplate,
		HasExternalRef: hasExternalRef,
		Fields:         rawFieldsFrom(descriptors),
		ReadyWhen:      readyWhen,
		IncludeWhen:    includeWhen,
		ForEach:        forEach,
	}, nil
}

func externalRefToTemplate(ref *v1alpha1.ExternalRef) map[string]interface{} {
	md := map[string]interface{}{"name": ref.Metadata.Name}
	if ref.Metadata.Namespace != "" {
		md["namespace"] = ref.Metadata.Namespace
	}
	return map[string]interface{}{
		"apiVersion": ref.APIVersion,
		"kind":       ref.Kind,
		"metadata":   md,
	}
}

func rawFieldsFrom(descriptors []variable.FieldDescriptor) []*RawField {
	out := make([]*RawField, 0, len(descriptors))
	for _, fd := range descriptors {
		exprs := make([]*RawExpr, len(fd.Expressions))
		for i, e := range fd.Expressions {
			exprs[i] = &RawExpr{Raw: e}
		}
		out = append(out, &RawField{
			Path:       fd.Path,
			Standalone: fd.StandaloneExpression,
			Exprs:      exprs,
		})
	}
	return out
}

func rawExprsFrom(exprs []string) []*RawExpr {
	out := make([]*RawExpr, len(exprs))
	for i, e := range exprs {
		out[i] = &RawExpr{Raw: e}
	}
	return out
}

func parseConditions(raw []string) ([]*RawExpr, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	celExprs, err := graphparser.ParseConditionExpressions(raw)
	if err != nil {
		return nil, err
	}
	return rawExprsFrom(celExprs), nil
}

func parseForEach(dims []v1alpha1.ForEachDimension) ([]*RawForEach, error) {
	if len(dims) == 0 {
		return nil, nil
	}
	out := make([]*RawForEach, 0, len(dims))
	for _, dim := range dims {
		for name, exprStr := range dim {
			celExprs, err := graphparser.ParseConditionExpressions([]string{exprStr})
			if err != nil {
				return nil, fmt.Errorf("dimension %q: %w", name, err)
			}
			out = append(out, &RawForEach{
				Name: name,
				Expr: &RawExpr{Raw: celExprs[0]},
			})
		}
	}
	return out, nil
}
