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

// Package generator provides small helpers for constructing v1alpha1.Graph
// objects in tests. Each option mutates the in-progress Graph; all options
// are composable.
package generator

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	expv1alpha1 "sigs.k8s.io/krocodile/api/v1alpha1"
)

// GraphOption is a functional option applied to an in-progress Graph.
type GraphOption func(*expv1alpha1.Graph)

// NewGraph constructs a Graph with the supplied name and options applied
// left-to-right. The Graph lives in no particular namespace by default.
func NewGraph(name string, opts ...GraphOption) *expv1alpha1.Graph {
	g := &expv1alpha1.Graph{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

// WithNamespace sets metadata.namespace.
func WithNamespace(ns string) GraphOption {
	return func(g *expv1alpha1.Graph) { g.ObjectMeta.Namespace = ns }
}

// WithTemplate appends a template node. forEach is optional and may be empty.
func WithTemplate(id string, template map[string]any, forEach ...expv1alpha1.ForEachDimension) GraphOption {
	return appendNode(func() expv1alpha1.Node {
		n := expv1alpha1.Node{
			ID:       id,
			Template: rawExt(template),
			ForEach:  forEach,
		}
		return n
	})
}

// WithRef appends a ref node from an ExternalRef.
func WithRef(id string, ref *expv1alpha1.ExternalRef) GraphOption {
	return appendNode(func() expv1alpha1.Node {
		return expv1alpha1.Node{ID: id, Ref: ref}
	})
}

// WithWatch appends a watch node from a WatchSpec.
func WithWatch(id string, watch *expv1alpha1.WatchSpec) GraphOption {
	return appendNode(func() expv1alpha1.Node {
		return expv1alpha1.Node{ID: id, Watch: watch}
	})
}

// WithDef appends a def node. The value is JSON-marshalled and embedded.
func WithDef(id string, def map[string]any) GraphOption {
	return appendNode(func() expv1alpha1.Node {
		return expv1alpha1.Node{ID: id, Def: rawExt(def)}
	})
}

// WithSubgraph appends a nested graph node whose child frame is the spec of
// the supplied Graph (its nodes list, marshalled into the node's payload).
func WithSubgraph(id string, child *expv1alpha1.Graph) GraphOption {
	return appendNode(func() expv1alpha1.Node {
		raw, err := json.Marshal(child.Spec)
		if err != nil {
			panic(err)
		}
		return expv1alpha1.Node{ID: id, Graph: &runtime.RawExtension{Raw: raw}}
	})
}

// WithRawNode appends the supplied node verbatim. Use this when a test needs
// to construct deliberately malformed input (e.g. zero or multiple kind
// fields set) that the higher-level helpers would refuse to produce.
func WithRawNode(n expv1alpha1.Node) GraphOption {
	return func(g *expv1alpha1.Graph) {
		g.Spec.Nodes = append(g.Spec.Nodes, n)
	}
}

// ForEachDim is a sugar for {name: expression} forEach entries.
func ForEachDim(name, expression string) expv1alpha1.ForEachDimension {
	return expv1alpha1.ForEachDimension{name: expression}
}

// WithReadyWhen appends readyWhen expressions to the most recently added
// node. No-op if no node has been added yet.
func WithReadyWhen(exprs ...string) GraphOption {
	return func(g *expv1alpha1.Graph) {
		if len(g.Spec.Nodes) == 0 {
			return
		}
		last := &g.Spec.Nodes[len(g.Spec.Nodes)-1]
		last.ReadyWhen = append(last.ReadyWhen, exprs...)
	}
}

// WithIncludeWhen appends includeWhen expressions to the most recently
// added node. No-op if no node has been added yet.
func WithIncludeWhen(exprs ...string) GraphOption {
	return func(g *expv1alpha1.Graph) {
		if len(g.Spec.Nodes) == 0 {
			return
		}
		last := &g.Spec.Nodes[len(g.Spec.Nodes)-1]
		last.IncludeWhen = append(last.IncludeWhen, exprs...)
	}
}

func appendNode(make func() expv1alpha1.Node) GraphOption {
	return func(g *expv1alpha1.Graph) {
		g.Spec.Nodes = append(g.Spec.Nodes, make())
	}
}

func rawExt(obj map[string]any) *runtime.RawExtension {
	raw, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}
	return &runtime.RawExtension{
		Object: &unstructured.Unstructured{Object: obj},
		Raw:    raw,
	}
}
