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
	"regexp"

	"k8s.io/apimachinery/pkg/util/sets"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
)

var (
	nodeIDRegex = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)

	// kubernetesVersionRegex matches `v1`, `v1alpha1`, `v2beta3`, etc.
	kubernetesVersionRegex = regexp.MustCompile(`^v\d+(?:(?:alpha|beta)\d+)?$`)

	// celReservedSymbols is the CEL grammar's reserved word list. No
	// identifier may collide with these — they would parse as keywords.
	celReservedSymbols = sets.NewString(
		"true", "false", "null", "in",
		"as", "break", "const", "continue", "else",
		"for", "function", "if", "import", "let",
		"loop", "package", "namespace", "return",
		"var", "void", "while",
	)
	// reservedNodeIDs are identifiers that the compiler or runtime claims
	// for itself — graph-level vocabulary that must not collide with user
	// node IDs. Keep this list to names we actually reference (or plan to
	// reference imminently); over-reservation breaks user graphs without
	// telling them why.
	reservedNodeIDs = sets.NewString(
		// Kubernetes manifest top-level fields and common subsections.
		"apiVersion", "kind", "metadata", "namespace", "spec", "status",
		// Project namespace.
		"graph", "graphengine", "kro",
		// CEL / runtime identifiers we wire in.
		"each", "item", "items", "object", "self", "this", "context",
	).Union(celReservedSymbols)
)

// validateGraph runs the structural checks that don't require CEL parsing.
// Per-node CEL extraction is done later by the compiler.
func validateGraph(g *expv1alpha1.Graph) error {
	if g == nil {
		return fmt.Errorf("graph is nil")
	}
	return validateFrameNodes(g.Spec.Nodes)
}

// validateFrameNodes runs the structural checks for one frame's node list —
// the top-level Graph or a nested subgraph. It is applied per frame as the
// compiler recurses into subgraphs.
func validateFrameNodes(nodes []expv1alpha1.Node) error {
	if len(nodes) == 0 {
		return fmt.Errorf("graph must declare at least one node")
	}

	seen := make(map[string]struct{}, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		if err := validateNodeID(n.ID); err != nil {
			return fmt.Errorf("node[%d]: %w", i, err)
		}
		if _, dup := seen[n.ID]; dup {
			return fmt.Errorf("duplicate node id %q", n.ID)
		}
		seen[n.ID] = struct{}{}
		if err := validateNodeShape(n); err != nil {
			return fmt.Errorf("node %q: %w", n.ID, err)
		}
		if err := validateForEachShape(n); err != nil {
			return fmt.Errorf("node %q: %w", n.ID, err)
		}
		if err := validateKindCompatibility(n); err != nil {
			return fmt.Errorf("node %q: %w", n.ID, err)
		}
	}
	return nil
}

// validateKindCompatibility rejects combinations that don't make sense for
// a given node kind. ref and watch nodes can't carry forEach because they
// don't render templates — they read existing state. graph (subgraph) nodes
// don't render or read; the per-node modifiers have no defined semantics on
// them yet, so they're rejected explicitly rather than silently ignored.
func validateKindCompatibility(n *expv1alpha1.Node) error {
	if n.Graph != nil {
		switch {
		case len(n.ForEach) > 0:
			return fmt.Errorf("forEach is not supported on graph nodes")
		case len(n.IncludeWhen) > 0:
			return fmt.Errorf("includeWhen is not supported on graph nodes")
		case len(n.ReadyWhen) > 0:
			return fmt.Errorf("readyWhen is not supported on graph nodes")
		}
		return nil
	}
	if len(n.ForEach) == 0 {
		return nil
	}
	switch {
	case n.Ref != nil:
		return fmt.Errorf("forEach is not supported on ref nodes")
	case n.Watch != nil:
		return fmt.Errorf("forEach is not supported on watch nodes (watch is inherently a collection)")
	}
	return nil
}

// validateNodeID checks that an ID is non-empty, matches the alphanumeric
// grammar, and is not a reserved word.
func validateNodeID(id string) error {
	if id == "" {
		return fmt.Errorf("id is required")
	}
	if !nodeIDRegex.MatchString(id) {
		return fmt.Errorf("id %q must match %s", id, nodeIDRegex.String())
	}
	if reservedNodeIDs.Has(id) {
		return fmt.Errorf("id %q is reserved", id)
	}
	return nil
}

// validateNodeShape enforces the discriminated-union contract: exactly one
// of template/patch/ref/watch/def must be set.
func validateNodeShape(n *expv1alpha1.Node) error {
	set := 0
	if n.Template != nil {
		set++
	}
	if n.Ref != nil {
		set++
	}
	if n.Watch != nil {
		set++
	}
	if n.Def != nil {
		set++
	}
	if n.Graph != nil {
		set++
	}
	if set != 1 {
		return fmt.Errorf("exactly one of template/ref/watch/def/graph must be set (got %d)", set)
	}
	return nil
}

// validateForEachShape checks that each forEach dimension has exactly one
// (name → expression) entry, and that iterator names don't collide with the
// node's own ID or with the reserved vocabulary.
func validateForEachShape(n *expv1alpha1.Node) error {
	seen := make(map[string]struct{}, len(n.ForEach))
	for i, dim := range n.ForEach {
		if len(dim) != 1 {
			return fmt.Errorf("forEach[%d]: must have exactly one entry, got %d", i, len(dim))
		}
		var name string
		for k := range dim {
			name = k
		}
		if name == "" {
			return fmt.Errorf("forEach[%d]: iterator name is empty", i)
		}
		if !nodeIDRegex.MatchString(name) {
			return fmt.Errorf("forEach[%d]: iterator name %q must match %s", i, name, nodeIDRegex.String())
		}
		if name == n.ID {
			return fmt.Errorf("forEach[%d]: iterator name %q collides with node id", i, name)
		}
		if reservedNodeIDs.Has(name) && name != EachVarName {
			return fmt.Errorf("forEach[%d]: iterator name %q is reserved", i, name)
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("forEach[%d]: duplicate iterator name %q", i, name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

// requireInputNode enforces that at least one compiled node has no CEL
// content. Without such a seed, the graph has no way to introduce data and
// any topology would either be empty or cyclic in scope.
func requireInputNode(nodes map[string]*Node) error {
	for _, n := range nodes {
		if len(n.Variables) == 0 && len(n.ForEach) == 0 {
			return nil
		}
	}
	return fmt.Errorf("graph must declare at least one node with no CEL expressions (an input/seed node)")
}
