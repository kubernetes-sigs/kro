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

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph/crd"
	graphschema "github.com/kubernetes-sigs/kro/pkg/graph/schema"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

type assembler struct{}

func newAssembler() Assembler { return &assembler{} }

// Assemble takes a CompiledRGD and produces the final Graph.
func (a *assembler) Assemble(compiled *CompiledRGD) (*Graph, error) {
	inst := compiled.Instance

	// Synthesize CRD — pass all metadata through SynthesizeCRD so the
	// CRD construction logic stays in one place (pkg/graph/crd).
	statusSchema, err := a.statusSchema(inst)
	if err != nil {
		return nil, terminal("assembler", fmt.Errorf("status schema: %w", err))
	}
	rgSchema := &v1alpha1.Schema{
		Kind:                     inst.Kind,
		APIVersion:               inst.APIVersion,
		Group:                    inst.Group,
		AdditionalPrinterColumns: inst.PrinterColumns,
		Metadata:                 inst.Metadata,
	}
	instanceCRD := crd.SynthesizeCRD(
		inst.Group, inst.APIVersion, inst.Kind,
		*inst.SpecSchema,
		*statusSchema,
		true, rgSchema,
	)

	// Build instance node
	instanceRes := a.buildInstanceNode(compiled)

	// Index nodes
	nodes := make(map[string]*CompiledNode, len(compiled.Nodes))
	for _, r := range compiled.Nodes {
		nodes[r.Meta.ID] = r
	}

	return &Graph{
		Instance:         instanceRes,
		Nodes:            nodes,
		DAG:              compiled.DAG,
		TopologicalOrder: compiled.TopologicalOrder,
		CRD:              instanceCRD,
	}, nil
}

func (a *assembler) statusSchema(inst *CompiledInstance) (*extv1.JSONSchemaProps, error) {
	if len(inst.StatusTypes) == 0 {
		return &extv1.JSONSchemaProps{Type: "object"}, nil
	}
	statusSchema, err := graphschema.GenerateSchemaFromCELTypes(inst.StatusTypes, inst.TypeProvider)
	if err != nil {
		return nil, err
	}
	return statusSchema, nil
}

func (a *assembler) buildInstanceNode(compiled *CompiledRGD) *CompiledNode {
	inst := compiled.Instance

	gvr := metadata.GetResourceGraphDefinitionInstanceGVR(inst.Group, inst.APIVersion, inst.Kind)

	// Prefix status field paths and collect dependencies
	fields := make([]*CompiledVariable, len(inst.StatusFields))
	deps := make([]string, 0)
	seenDeps := make(map[string]struct{})
	for i, f := range inst.StatusFields {
		fields[i] = &CompiledVariable{
			Path:       "status." + f.Path,
			Standalone: f.Standalone,
			Kind:       FieldDynamic,
			Exprs:      f.Exprs,
		}
		for _, expr := range f.Exprs {
			for _, ref := range expr.References {
				if ref != SchemaVarName {
					if _, ok := seenDeps[ref]; ok {
						continue
					}
					seenDeps[ref] = struct{}{}
					deps = append(deps, ref)
				}
			}
		}
	}

	return &CompiledNode{
		Meta: CompiledNodeMeta{
			ID:           SchemaVarName,
			Type:         NodeTypeInstance,
			GVR:          gvr,
			Namespaced:   true,
			Dependencies: deps,
		},
		Template: map[string]interface{}{
			"status": inst.StatusTemplate,
		},
		Variables: fields,
	}
}
