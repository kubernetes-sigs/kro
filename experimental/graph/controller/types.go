// types.go defines the Graph data model and spec parsing.
//
// Node and GraphSpec are the parsed representations of a Graph's spec.
// Everything else in the package depends on these types; they don't depend
// on anything else in the package.
package graphcontroller

import "fmt"

// TemplateShape classifies the relationship between a Graph and a resource.
// Determined by template content — see 003-ownership.md § Template Shapes.
type TemplateShape int

const (
	// ShapeOwns — the template specifies fields beyond identity; the controller
	// creates/owns the resource via SSA. Tracked for cleanup. Deleted on prune.
	ShapeOwns TemplateShape = iota
	// ShapeWatch — identity-only template (apiVersion, kind, metadata.name,
	// optionally metadata.namespace). Read-only GET. Not tracked.
	ShapeWatch
	// ShapeCollectionWatch — apiVersion + kind with optional selector, no
	// metadata.name. Read-only List. Not tracked.
	ShapeCollectionWatch
	// ShapeContribute — specifies fields on a resource the Graph does not
	// create. Applied via SSA. Tracked for cleanup. Releases fields on prune.
	ShapeContribute
)

// String returns the human-readable name of the TemplateShape.
func (s TemplateShape) String() string {
	switch s {
	case ShapeOwns:
		return "Owns"
	case ShapeWatch:
		return "Watch"
	case ShapeCollectionWatch:
		return "CollectionWatch"
	case ShapeContribute:
		return "Contribute"
	default:
		return fmt.Sprintf("TemplateShape(%d)", int(s))
	}
}

// DetectShape returns the TemplateShape of a node's template map.
//
// Detection order (from 003-ownership.md):
//  1. CollectionWatch — apiVersion + kind, no metadata.name
//  2. Watch — only identity fields (apiVersion, kind, metadata.name/namespace)
//  3. Contribute — keys subset of {apiVersion, kind, metadata, status}
//  4. Owns — fields beyond identity
func DetectShape(tmpl map[string]any) TemplateShape {
	if len(tmpl) == 0 {
		return ShapeOwns
	}

	md, _ := tmpl["metadata"].(map[string]any)
	_, hasName := md["name"]

	// 1. Collection Watch: no metadata.name
	if !hasName {
		return ShapeCollectionWatch
	}

	// 2. Watch: only identity fields
	if isIdentityOnly(tmpl) {
		return ShapeWatch
	}

	// 3. Contribute: keys subset of {apiVersion, kind, metadata, status}
	if isContributeShape(tmpl) {
		return ShapeContribute
	}

	return ShapeOwns
}

// isIdentityOnly returns true if the template contains only identity fields:
// apiVersion, kind, and metadata with only name and/or namespace.
func isIdentityOnly(tmpl map[string]any) bool {
	for key := range tmpl {
		switch key {
		case "apiVersion", "kind", "metadata":
			continue
		default:
			return false
		}
	}
	md, _ := tmpl["metadata"].(map[string]any)
	if md == nil {
		return true
	}
	for key := range md {
		switch key {
		case "name", "namespace":
			continue
		default:
			return false
		}
	}
	return true
}

// isContributeShape returns true if the template's keys are a subset of
// {apiVersion, kind, metadata, status} — it underspecifies the resource,
// writing only metadata and/or status fields.
func isContributeShape(tmpl map[string]any) bool {
	if len(tmpl) == 0 {
		return false
	}
	for key := range tmpl {
		switch key {
		case "apiVersion", "kind", "metadata", "status":
			continue
		default:
			return false
		}
	}
	return true
}

// Node is a parsed Graph node entry — a user's declaration of intent about
// a Kubernetes resource (or collection of resources via forEach). It is the
// unit of the dependency graph: each node has an identity, a template, and
// computed dependency edges populated by BuildDAG.
//
// "Node" (not "Resource") because a node is a graph-theory concept — it
// occupies a position in the DAG, has edges, and may produce zero, one, or
// many Kubernetes resources at runtime (e.g., forEach). Kubernetes already
// uses "resource" for five distinct concepts; adding a sixth blurs the
// boundary between the user's declaration and the Kubernetes objects it
// produces.
type Node struct {
	ID            string
	Template      map[string]any
	ForEach       map[string]string
	IncludeWhen   []string
	ReadyWhen     []string // CEL conditions; all must be true for the node to be "ready"
	PropagateWhen []string // CEL conditions; all must be true for data to flow to dependents

	// Dependencies are IDs of nodes this node references in its CEL expressions.
	// Populated by BuildDAG; nil before that.
	Dependencies map[string]bool
}

// Shape returns the TemplateShape of this node's template.
func (n *Node) Shape() TemplateShape {
	return DetectShape(n.Template)
}

// GraphSpec holds the parsed spec of a Graph object.
type GraphSpec struct {
	Nodes []Node
}

// AllIdentifiers returns every identifier that CEL expressions in this spec
// might reference: node IDs and forEach iterator variable names.
// Used to build the CEL environment with all variables declared upfront.
func (s *GraphSpec) AllIdentifiers() []string {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, node := range s.Nodes {
		add(node.ID)
		for varName := range node.ForEach {
			add(varName)
		}
	}
	return ids
}

// AllExpressions returns every CEL expression string in the spec.
// This is the single source of truth for what expressions exist in a Graph.
// The compilation phase uses this to eagerly compile all programs before
// the reconcile loop touches any nodes.
func (s *GraphSpec) AllExpressions() []string {
	seen := map[string]bool{}
	var exprs []string

	add := func(strs []string) {
		for _, s := range strs {
			// Extract ${...} expressions from each string
			pos := 0
			for {
				dollars, expr, start, _ := findExpr(s, pos)
				if start < 0 {
					break
				}
				pos = start + len(dollars) + len(expr) + 2
				if len(dollars) != 1 {
					continue // $${...} is deferred, not evaluated at this level
				}
				if !seen[expr] {
					seen[expr] = true
					exprs = append(exprs, expr)
				}
			}
		}
	}

	// Collect expressions from each node
	for _, node := range s.Nodes { // Template expressions
		var templateStrings []string
		collectStrings(node.Template, &templateStrings)
		add(templateStrings)

		// ForEach collection expressions
		for _, v := range node.ForEach {
			add([]string{v})
		}

		// Condition expressions (includeWhen, readyWhen, propagateWhen)
		add(node.IncludeWhen)
		add(node.ReadyWhen)
		add(node.PropagateWhen)
	}

	return exprs
}

// extractGraphSpec parses the spec from a Graph object.
func extractGraphSpec(graphObj map[string]any) (*GraphSpec, error) {
	spec, ok := graphObj["spec"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("missing spec")
	}
	rawNodes, ok := spec["nodes"]
	if !ok {
		return nil, fmt.Errorf("missing spec.nodes")
	}
	nodes, err := parseNodeList(rawNodes)
	if err != nil {
		return nil, err
	}

	return &GraphSpec{Nodes: nodes}, nil
}

// parseNodeList converts a raw node list into Nodes.
// Returns an error if any node is missing an ID or has a duplicate ID.
func parseNodeList(raw any) ([]Node, error) {
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("spec.nodes is %T, want []any", raw)
	}

	seen := make(map[string]bool, len(list))
	var nodes []Node
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("node[%d] is %T, want map", i, item)
		}

		node := Node{}
		id, ok := m["id"].(string)
		if !ok || id == "" {
			return nil, fmt.Errorf("node[%d]: missing or empty id", i)
		}
		if seen[id] {
			return nil, fmt.Errorf("node[%d]: duplicate id %q", i, id)
		}
		seen[id] = true
		node.ID = id
		if tmpl, ok := m["template"].(map[string]any); ok {
			node.Template = tmpl
		}
		if fe, ok := m["forEach"].(map[string]any); ok {
			node.ForEach = make(map[string]string)
			for k, v := range fe {
				if vs, ok := v.(string); ok {
					node.ForEach[k] = vs
				}
			}
		}
		if iw, ok := m["includeWhen"].([]any); ok {
			for _, expr := range iw {
				if s, ok := expr.(string); ok {
					node.IncludeWhen = append(node.IncludeWhen, s)
				}
			}
		}
		if rw, ok := m["readyWhen"].([]any); ok {
			for _, expr := range rw {
				if s, ok := expr.(string); ok {
					node.ReadyWhen = append(node.ReadyWhen, s)
				}
			}
		}
		if pw, ok := m["propagateWhen"].([]any); ok {
			for _, expr := range pw {
				if s, ok := expr.(string); ok {
					node.PropagateWhen = append(node.PropagateWhen, s)
				}
			}
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}
