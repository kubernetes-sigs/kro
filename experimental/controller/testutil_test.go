package graphcontroller

import "github.com/ellistarn/kro/experimental/controller/graph"

// node constructs a graph.Node and sets its classification type.
// This is a test helper for building Node struct literals without
// accessing the unexported nodeType field.
func node(n graph.Node, t graph.NodeType) graph.Node {
	n.SetType(t)
	return n
}
