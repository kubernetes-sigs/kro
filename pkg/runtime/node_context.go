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

package runtime

import (
	"maps"
	"slices"

	"k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/kube-openapi/pkg/validation/spec"

	celunstructured "github.com/kubernetes-sigs/kro/pkg/cel/unstructured"
	"github.com/kubernetes-sigs/kro/pkg/graph"
)

// buildContext builds the CEL activation context from node dependencies.
// If only is provided, only those dependency IDs are included in the context.
// If only is empty/nil, all dependencies are included.
//
// When a dependency has a resourceSchema, its observed objects are wrapped using
// Kubernetes' UnstructuredToVal for schema-aware type conversion. This ensures
// CEL runtime values match their compile-time types (e.g., Secret data as bytes).
func (n *Node) buildContext(only ...string) map[string]any {
	ctx := make(map[string]any)
	for depID, dep := range n.deps {
		// Use nil check (not len==0) to include empty collections in context.
		if dep.observed == nil {
			continue
		}
		if len(only) > 0 && !slices.Contains(only, depID) {
			continue
		}
		if dep.Spec.Meta.Type == graph.NodeTypeCollection || dep.Spec.Meta.Type == graph.NodeTypeExternalCollection {
			items := make([]any, len(dep.observed))
			for i, obj := range dep.observed {
				items[i] = wrapWithSchema(obj.Object, dep.resourceSchema)
			}
			ctx[depID] = items
		} else {
			obj := dep.observed[0].Object
			// For schema (instance), strip status - users should only access spec/metadata.
			// The instance's resourceSchema already excludes status (set by builder via
			// getSchemaWithoutStatus), so the schema and data stay aligned.
			if depID == graph.InstanceNodeID {
				obj = withStatusOmitted(obj)
			}
			ctx[depID] = wrapWithSchema(obj, dep.resourceSchema)
		}
	}
	return ctx
}

// wrapWithSchema wraps an unstructured object with schema-aware CEL value
// conversion. If the schema is nil, the raw object is returned. Otherwise,
// returns a schemaMap that delegates to UnstructuredToVal for typed properties
// and falls back to NativeToValue for preserve-unknown fields.
func wrapWithSchema(obj map[string]interface{}, schema *spec.Schema) any {
	if schema == nil {
		return obj
	}
	return celunstructured.UnstructuredToVal(obj, &openapi.Schema{Schema: schema})
}

// withStatusOmitted returns a shallow copy of obj with the "status" key removed.
// This prevents CEL expressions from accessing instance status fields.
func withStatusOmitted(obj map[string]any) map[string]any {
	result := make(map[string]any, len(obj))
	for k, v := range obj {
		if k != "status" {
			result[k] = v
		}
	}
	return result
}

// neededDeps computes the union of referenced dependency IDs across the
// expressions in the given set. If exprs is nil, all non-iteration template
// expressions are included. This allows buildContext to only wrap needed deps.
func (n *Node) neededDeps(exprs map[string]struct{}) []string {
	needed := make(map[string]struct{})
	for _, expr := range n.templateExprs {
		if expr.Kind.IsIteration() {
			continue
		}
		if exprs != nil {
			if _, ok := exprs[expr.Expression.Original]; !ok {
				continue
			}
		}
		for _, ref := range expr.Expression.References {
			needed[ref] = struct{}{}
		}
	}
	return slices.Collect(maps.Keys(needed))
}

// contextDependencyIDs returns CEL variable names grouped by type.
// - singles: dependencies that are single resources (declared as dyn)
// - collections: dependencies that are collections (declared as list(dyn))
// - iterators: forEach loop variable names from iterCtx (declared as list(dyn))
func (n *Node) contextDependencyIDs(iterCtx map[string]any) (singles, collections, iterators []string) {
	for depID, dep := range n.deps {
		if dep.Spec.Meta.Type == graph.NodeTypeCollection || dep.Spec.Meta.Type == graph.NodeTypeExternalCollection {
			collections = append(collections, depID)
		} else {
			singles = append(singles, depID)
		}
	}
	for name := range iterCtx {
		iterators = append(iterators, name)
	}
	return
}
