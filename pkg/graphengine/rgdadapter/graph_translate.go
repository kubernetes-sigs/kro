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

// Package rgdadapter translates a ResourceGraphDefinition into a Graph so
// RGD composition can run on the Graph engine (compiler.Compile →
// runtime.New → executor.Apply). Template resources and named externalRef →
// ref are mapped to nodes; the instance spec is not a resource node and is
// injected as a `schema` def node via InstanceSchemaNode.
package rgdadapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// ErrUnsupported is returned when an RGD resource shape has no Graph-node
// mapping. The error names the gap rather than silently dropping the
// resource.
var ErrUnsupported = errors.New("rgdadapter: unsupported RGD shape")

// ResourceGraphDefinitionToGraph maps each RGD resource onto a Graph node:
//
//   - template          → Node.Template (same ID, raw manifest + ${...} CEL intact)
//   - named externalRef → Node.Ref
//
// forEach / readyWhen / includeWhen are copied through. The instance
// spec is not emitted as a resource node — callers prepend InstanceSchemaNode
// to expose it as `${schema.spec.*}`.
//
// When the RGD declares author status fields, a synthesized patch node
// (StatusPatchNodeID) is appended to write them onto the instance's status
// subresource; see authorStatusPatchNode.
func ResourceGraphDefinitionToGraph(rgd *v1alpha1.ResourceGraphDefinition) (*v1alpha1.Graph, error) {
	if rgd == nil {
		return nil, fmt.Errorf("rgdadapter: resourcegraphdefinition is required")
	}
	// Zero resources is valid: a NoOp/arbitrary-object RGD manages no
	// children and only projects status from its schema. The instance's
	// `schema` def node (prepended by BuildRuntimeForInstance) keeps the
	// compiled Graph non-empty, so the MinItems=1 Node constraint still holds.

	g := &v1alpha1.Graph{}
	g.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("Graph"))
	g.Name = rgd.Name
	g.Spec.Nodes = make([]v1alpha1.Node, 0, len(rgd.Spec.Resources))

	for i, res := range rgd.Spec.Resources {
		if res == nil {
			return nil, fmt.Errorf("rgdadapter: resource[%d]: resource is nil", i)
		}
		node, err := resourceToNode(res)
		if err != nil {
			return nil, err
		}
		g.Spec.Nodes = append(g.Spec.Nodes, node)
	}

	// Synthesize the author-status writeback node: a patch node that writes
	// the RGD's author status FIELDS onto the instance's status subresource.
	// kro-owned conditions, author conditions, and .status.state stay
	// controller-side. Absent when the RGD declares no author status fields.
	statusNode, ok, err := authorStatusPatchNode(rgd)
	if err != nil {
		return nil, err
	}
	if ok {
		g.Spec.Nodes = append(g.Spec.Nodes, statusNode)
	}
	return g, nil
}

// StatusPatchNodeID is the node ID of the synthesized author-status writeback
// patch node. `instance` is reserved at the RGD level (kroReservedKeyWords),
// so no user resource can collide, and it is not reserved by the Graph
// compiler, so the synthesized node validates — the same split the `schema`
// def node relies on.
const StatusPatchNodeID = "instance"

// authorStatusPatchNode builds the patch node that writes the RGD's author
// status FIELDS (spec.schema.status minus the conditions block) onto the
// instance's status subresource. It returns ok=false when the RGD declares no
// author status fields or no target kind.
//
// The node targets the instance GVK (Schema.Group/APIVersion/Kind), keys the
// target by ${schema.metadata.name} (+ namespace for namespaced instances),
// and carries the author fields verbatim under the manifest's top-level
// status so their ${...} CEL is compiled and type-checked like any patch
// manifest. BuildRuntimeForInstance marks this node soft-deps +
// per-field-tolerant so it never gates on the resources it reads and omits
// unresolved fields (mirroring ProjectInstanceStatus's per-field progressive
// projection).
func authorStatusPatchNode(rgd *v1alpha1.ResourceGraphDefinition) (v1alpha1.Node, bool, error) {
	if rgd.Spec.Schema == nil {
		return v1alpha1.Node{}, false, nil
	}
	raw := rgd.Spec.Schema.Status.Raw
	if len(raw) == 0 {
		return v1alpha1.Node{}, false, nil
	}
	var statusMap map[string]any
	if err := json.Unmarshal(raw, &statusMap); err != nil {
		return v1alpha1.Node{}, false, fmt.Errorf("rgdadapter: unmarshal status: %w", err)
	}
	// Author conditions stay controller-side (ProjectInstanceConditions), and
	// .status.state is projected by the controller under its own field manager
	// (kro-instance-status); leaving either in the synthesized node's payload
	// makes two Force:true SSA writers fight over the same field forever. Only
	// the remaining author status fields move to the node.
	delete(statusMap, "conditions")
	delete(statusMap, "state")
	if len(statusMap) == 0 {
		return v1alpha1.Node{}, false, nil
	}

	kind := rgd.Spec.Schema.Kind
	if kind == "" {
		// No target kind (a malformed or test-only schema); nothing to write.
		return v1alpha1.Node{}, false, nil
	}
	group := rgd.Spec.Schema.Group
	if group == "" {
		group = "kro.run"
	}
	apiVersion := group + "/" + rgd.Spec.Schema.APIVersion
	namespaced := rgd.Spec.Schema.Scope != v1alpha1.ResourceScopeCluster

	metadata := map[string]any{"name": "${schema.metadata.name}"}
	if namespaced {
		metadata["namespace"] = "${schema.metadata.namespace}"
	}
	manifest := map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   metadata,
		"status":     statusMap,
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return v1alpha1.Node{}, false, fmt.Errorf("rgdadapter: marshal status patch manifest: %w", err)
	}
	return v1alpha1.Node{ID: StatusPatchNodeID, Patch: &runtime.RawExtension{Raw: raw}}, true, nil
}

func resourceToNode(res *v1alpha1.Resource) (v1alpha1.Node, error) {
	hasTemplate := len(res.Template.Raw) > 0
	hasRef := res.ExternalRef != nil
	switch {
	case hasTemplate && hasRef:
		return v1alpha1.Node{}, fmt.Errorf("%w: resource %q: template and externalRef are both set", ErrUnsupported, res.ID)
	case hasTemplate:
		return v1alpha1.Node{
			ID:          res.ID,
			Template:    copyRaw(res.Template.Raw),
			ReadyWhen:   copyStrings(res.ReadyWhen),
			IncludeWhen: copyStrings(res.IncludeWhen),
			ForEach:     copyForEach(res.ForEach),
		}, nil
	case hasRef:
		// A selector externalRef is a read-only COLLECTION of external
		// objects: name is absent (mutually exclusive with selector at the
		// API level), and the compiler/executor treat the node as a
		// collection. A single-object externalRef requires metadata.name.
		isCollection := res.ExternalRef.Metadata.HasSelector()
		if !isCollection && res.ExternalRef.Metadata.Name == "" {
			return v1alpha1.Node{}, fmt.Errorf("%w: resource %q: externalRef is missing metadata.name", ErrUnsupported, res.ID)
		}
		if len(res.ForEach) > 0 {
			return v1alpha1.Node{}, fmt.Errorf(
				"%w: resource %q: forEach on externalRef is not supported",
				ErrUnsupported,
				res.ID,
			)
		}
		// The ExternalRef (including the selector under metadata.selector as a
		// runtime.RawExtension — a literal LabelSelector with matchLabels +
		// matchExpressions, or a full CEL expression resolving to one) is
		// carried through verbatim into the Ref node. projectPayload converts
		// it to unstructured, so CEL fragments inside the selector are parsed
		// and rendered, and the executor can read the selector back off the
		// resolved object.
		return v1alpha1.Node{
			ID:          res.ID,
			Ref:         res.ExternalRef.DeepCopy(),
			ReadyWhen:   copyStrings(res.ReadyWhen),
			IncludeWhen: copyStrings(res.IncludeWhen),
		}, nil
	default:
		return v1alpha1.Node{}, fmt.Errorf("%w: resource %q: neither template nor externalRef is set", ErrUnsupported, res.ID)
	}
}

func copyRaw(raw []byte) *runtime.RawExtension {
	return &runtime.RawExtension{Raw: append([]byte(nil), raw...)}
}

// SchemaNodeID is the node ID under which an instance's spec/metadata/status is
// exposed in Graph scope, matching RGD's `schema` variable.
const SchemaNodeID = "schema"

// InstanceSchemaNode materialises an instance object as a Graph `def` node named
// `schema`, so RGD-style `${schema.spec.*}` references resolve in the Graph
// world. The Graph compiler rejects unknown top-level identifiers, so the
// instance must be a declared node (a def) rather than post-compile seeded
// scope. Prepend the returned node to a translated Graph for each instance
// reconcile.
func InstanceSchemaNode(instance *unstructured.Unstructured) (v1alpha1.Node, error) {
	val, err := instanceSchemaValue(instance)
	if err != nil {
		return v1alpha1.Node{}, err
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return v1alpha1.Node{}, fmt.Errorf("rgdadapter: marshal instance schema node: %w", err)
	}
	return v1alpha1.Node{ID: SchemaNodeID, Def: &runtime.RawExtension{Raw: raw}}, nil
}

// instanceSchemaValue projects the instance's metadata/spec/status into the
// value map exposed under `${schema.*}`. It is the shared source for both the
// baked-in InstanceSchemaNode (legacy path) and the runtime-injected schema
// data used by the cached compile path (BuildRuntimeForInstanceCached), so the
// two paths expose byte-identical instance scope.
func instanceSchemaValue(instance *unstructured.Unstructured) (map[string]any, error) {
	if instance == nil {
		return nil, fmt.Errorf("rgdadapter: instance is required")
	}
	val := make(map[string]any, 5)
	if av := instance.GetAPIVersion(); av != "" {
		val["apiVersion"] = av
	} else if av, ok := instance.Object["apiVersion"]; ok {
		val["apiVersion"] = av
	}
	if k := instance.GetKind(); k != "" {
		val["kind"] = k
	} else if k, ok := instance.Object["kind"]; ok {
		val["kind"] = k
	}
	// Expose the instance's full metadata (uid, labels, annotations,
	// generation, creationTimestamp, …) as ${schema.metadata.*}. Falling
	// back to name/namespace only would break RGDs that reference e.g.
	// ${schema.metadata.uid}.
	if md, ok := instance.Object["metadata"].(map[string]any); ok {
		val["metadata"] = md
	} else {
		val["metadata"] = map[string]any{
			"name":      instance.GetName(),
			"namespace": instance.GetNamespace(),
		}
	}
	if spec, ok := instance.Object["spec"]; ok {
		val["spec"] = spec
	}
	if status, ok := instance.Object["status"]; ok {
		val["status"] = status
	}
	return val, nil
}

// emptySchemaNode returns a `schema` def node with an empty payload. Its TYPE
// is supplied at compile time via compiler.WithNodeSchemaOverride (from the
// RGD's declared SimpleSchema), not inferred from any instance value, so the
// compiled Program is identical for every instance of a revision and therefore
// cacheable. The per-instance data is injected at runtime via
// runtime.WithNodeObjectOverride rather than baked in here.
func emptySchemaNode() v1alpha1.Node {
	return v1alpha1.Node{ID: SchemaNodeID, Def: &runtime.RawExtension{Raw: []byte("{}")}}
}

func copyStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return append([]string(nil), in...)
}

func copyForEach(in []v1alpha1.ForEachDimension) []v1alpha1.ForEachDimension {
	if len(in) == 0 {
		return nil
	}
	out := make([]v1alpha1.ForEachDimension, len(in))
	for i, dim := range in {
		copied := make(v1alpha1.ForEachDimension, len(dim))
		maps.Copy(copied, dim)
		out[i] = copied
	}
	return out
}
