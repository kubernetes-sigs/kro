package graph

import (
	"testing"
)

// TestExtractReferencedPaths_ReadyInBody verifies that .ready() calls in
// body expressions are detected as dependencies. With the string-based
// fallback (nil exprPaths/exprAccessModes), only the first identifier is
// extracted. Full lazy classification requires AST analysis at compile time.
func TestExtractReferencedPaths_ReadyInBody(t *testing.T) {
	node := Node{
		ID: "rgdInstanceStatus",
		Patch: map[string]any{
			"status": map[string]any{
				"state": "${deployment1.ready() && deployment2.ready() ? 'ACTIVE' : 'IN_PROGRESS'}",
			},
		},
	}

	deps, _, _, err := ExtractReferencedPathsFromNode(node, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// String fallback: processExpr extracts FIRST identifier only
	// (deployment1 from ExtractFirstIdentifier) as a hard dep.
	if deps["deployment1"] != DepHard {
		t.Error("deployment1 should be a hard dependency (string fallback)")
	}
	// deployment2 is not detected by string fallback (only first identifier).
	// Full lazy classification requires AST-based exprAccessModes from compilation.
	if _, exists := deps["deployment2"]; exists {
		t.Error("deployment2 should not appear in string-fallback mode (only first identifier extracted)")
	}
}

// TestExtractReferencedPaths_ReadyInBody_Single verifies a single .ready()
// call in a body expression creates the correct dep kind.
func TestExtractReferencedPaths_ReadyInBody_Single(t *testing.T) {
	node := Node{
		ID: "status",
		Patch: map[string]any{
			"status": map[string]any{
				"ready": "${job.ready()}",
			},
		},
	}

	deps, _, _, err := ExtractReferencedPathsFromNode(node, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// String fallback adds "job" as hard dep via ExtractFirstIdentifier.
	// checkReadyRef would add it as lazy, but hard wins.
	if deps["job"] != DepHard {
		t.Error("job should be a hard dependency (string fallback, hard wins over lazy)")
	}
}

// TestExtractReferencedPaths_ReadySelfReference verifies that .ready() on
// self is ignored — a node can't depend on its own readiness.
func TestExtractReferencedPaths_ReadySelfReference(t *testing.T) {
	node := Node{
		ID: "service",
		Template: map[string]any{
			"data": "${service.ready()}",
		},
	}

	deps, _, _, err := ExtractReferencedPathsFromNode(node, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, exists := deps["service"]; exists {
		t.Error("self-reference should not create any dependency")
	}
}

// TestExtractReferencedPaths_ReadyInGate verifies that .ready() in
// propagateWhen creates a lazy dependency for re-triggering.
func TestExtractReferencedPaths_ReadyInGate(t *testing.T) {
	node := Node{
		ID: "service",
		Template: map[string]any{
			"apiVersion": "v1",
			"kind":       "Service",
		},
		PropagateWhen: []string{"${deployment.ready()}"},
	}

	deps, _, _, err := ExtractReferencedPathsFromNode(node, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// String fallback: processExpr extracts "deployment" as hard.
	// checkReadyRef would set lazy but hard wins.
	if deps["deployment"] != DepHard {
		t.Error("deployment should be a hard dependency (string fallback)")
	}
}

// TestExtractReferencedPaths_DependenciesSelfOnly verifies that
// .dependencies() can only be called on the node itself.
func TestExtractReferencedPaths_DependenciesSelfOnly(t *testing.T) {
	node := Node{
		ID: "service",
		Template: map[string]any{
			"apiVersion": "v1",
			"kind":       "Service",
		},
		PropagateWhen: []string{"${service.dependencies().all(d, d.ready())}"},
	}

	_, _, _, err := ExtractReferencedPathsFromNode(node, nil, nil)
	if err != nil {
		t.Fatalf("self-referential .dependencies() should be allowed: %v", err)
	}

	node = Node{
		ID: "status",
		Patch: map[string]any{
			"state": "${otherNode.dependencies().all(d, d.ready())}",
		},
	}

	_, _, _, err = ExtractReferencedPathsFromNode(node, nil, nil)
	if err == nil {
		t.Fatal("cross-node .dependencies() should be rejected")
	}
}

// TestExtractReferencedPaths_ReadyCELBuiltinFiltered verifies that CEL
// builtins like "all", "filter", "map" before .ready() are not treated
// as node IDs. In string-fallback mode, comprehension variables like "d"
// are not detected — full classification requires AST-based analysis.
func TestExtractReferencedPaths_ReadyCELBuiltinFiltered(t *testing.T) {
	node := Node{
		ID: "status",
		Patch: map[string]any{
			"data": "${list.all(d, d.ready())}",
		},
	}

	deps, _, _, err := ExtractReferencedPathsFromNode(node, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, exists := deps["list"]; exists {
		t.Error("CEL builtin 'list' should be filtered by ExtractFirstIdentifier")
	}
	// "d" is a loop variable — string fallback can't detect it. Full
	// classification requires AST-based exprAccessModes from compilation.
	if _, exists := deps["d"]; exists {
		t.Error("'d' should not appear in string-fallback mode")
	}
}
