package graph

import (
	"testing"
)

// TestExtractReferencedPaths_ReadyInBody_RegressionMultiTarget verifies that
// .ready() calls in body expressions create readinessDeps entries for ALL
// targets. With exprPaths == nil (string fallback), processExpr also adds
// identifiers to dependencies (conservative). With exprPaths != nil (AST
// path in production), the AST walker skips .ready() targets, so only
// readinessDeps are populated — no hard DAG edges.
func TestExtractReferencedPaths_ReadyInBody_RegressionMultiTarget(t *testing.T) {
	// Simulates rgdInstanceStatus's body expression:
	//   ${deployment1.ready() && deployment2.ready() ? 'ACTIVE' : 'IN_PROGRESS'}
	node := Node{
		ID: "rgdInstanceStatus",
		Patch: map[string]any{
			"status": map[string]any{
				"state": "${deployment1.ready() && deployment2.ready() ? 'ACTIVE' : 'IN_PROGRESS'}",
			},
		},
	}

	deps, _, _, readinessDeps, err := ExtractReferencedPathsFromNode(node, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// String fallback: processExpr extracts FIRST identifier only
	// (deployment1 from ExtractFirstIdentifier). checkReadyRef does NOT
	// add to deps for body. deployment2 is NOT in deps because
	// ExtractFirstIdentifier only finds the first.
	if !deps["deployment1"] {
		t.Error("deployment1 missing from dependencies (string fallback extracts first identifier)")
	}
	// deployment2 is NOT extracted by ExtractFirstIdentifier
	if deps["deployment2"] {
		t.Error("deployment2 should not be in dependencies (ExtractFirstIdentifier only finds first)")
	}

	// Both targets must appear in readinessDeps (propagation on readiness change).
	if !readinessDeps["deployment1"] {
		t.Error("deployment1 missing from readinessDeps")
	}
	if !readinessDeps["deployment2"] {
		t.Error("deployment2 missing from readinessDeps")
	}
}

// TestExtractReferencedPaths_ReadyInBody_RegressionSingle verifies the simple
// case: a single .ready() call in a body expression creates readinessDeps.
// With string fallback (exprPaths == nil), processExpr also adds deps.
func TestExtractReferencedPaths_ReadyInBody_RegressionSingle(t *testing.T) {
	node := Node{
		ID: "status",
		Patch: map[string]any{
			"status": map[string]any{
				"ready": "${job.ready()}",
			},
		},
	}

	deps, _, _, readinessDeps, err := ExtractReferencedPathsFromNode(node, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// String fallback adds all identifiers to deps (conservative).
	if !deps["job"] {
		t.Error("job missing from dependencies (string fallback)")
	}
	if !readinessDeps["job"] {
		t.Error("job missing from readinessDeps")
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

	deps, _, _, readinessDeps, err := ExtractReferencedPathsFromNode(node, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if deps["service"] {
		t.Error("self-reference should not create a dependency")
	}
	if readinessDeps["service"] {
		t.Error("self-reference should not create a readinessDep")
	}
}

// TestExtractReferencedPaths_ReadyInGate verifies that .ready() in
// propagateWhen/readyWhen still works (was already working before the fix).
func TestExtractReferencedPaths_ReadyInGate(t *testing.T) {
	node := Node{
		ID: "service",
		Template: map[string]any{
			"apiVersion": "v1",
			"kind":       "Service",
		},
		PropagateWhen: []string{"${deployment.ready()}"},
	}

	deps, _, _, readinessDeps, err := ExtractReferencedPathsFromNode(node, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !deps["deployment"] {
		t.Error("deployment missing from dependencies")
	}
	if !readinessDeps["deployment"] {
		t.Error("deployment missing from readinessDeps")
	}
}

// TestExtractReferencedPaths_DependenciesSelfOnly verifies that
// .dependencies() can only be called on the node itself.
func TestExtractReferencedPaths_DependenciesSelfOnly(t *testing.T) {
	// Self-reference should be allowed.
	node := Node{
		ID: "service",
		Template: map[string]any{
			"apiVersion": "v1",
			"kind":       "Service",
		},
		PropagateWhen: []string{"${service.dependencies().all(d, d.ready())}"},
	}

	_, _, _, _, err := ExtractReferencedPathsFromNode(node, nil)
	if err != nil {
		t.Fatalf("self-referential .dependencies() should be allowed: %v", err)
	}

	// Cross-node reference should be rejected.
	node = Node{
		ID: "status",
		Patch: map[string]any{
			"state": "${otherNode.dependencies().all(d, d.ready())}",
		},
	}

	_, _, _, _, err = ExtractReferencedPathsFromNode(node, nil)
	if err == nil {
		t.Fatal("cross-node .dependencies() should be rejected")
	}
}

// TestExtractReferencedPaths_ReadyCELBuiltinFiltered verifies that CEL
// builtins like "all", "filter", "map" before .ready() are not treated
// as node IDs. Note: loop variables like "d" in comprehensions CANNOT
// be distinguished from node IDs by the string-based scanner — this is
// a known limitation. The AST-based path (exprPaths != nil) handles
// this correctly; the string fallback (exprPaths == nil) is best-effort.
func TestExtractReferencedPaths_ReadyCELBuiltinFiltered(t *testing.T) {
	node := Node{
		ID: "status",
		Patch: map[string]any{
			// "all" is a CEL builtin, not a node ID
			"data": "${list.all(d, d.ready())}",
		},
	}

	deps, _, _, readinessDeps, err := ExtractReferencedPathsFromNode(node, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// "list" is filtered as a CEL builtin by ExtractFirstIdentifier.
	if deps["list"] {
		t.Error("CEL builtin 'list' should be filtered by ExtractFirstIdentifier")
	}
	// "d" is a loop variable — the string scanner can't distinguish it
	// from a node ID. With exprPaths == nil, ExtractFirstIdentifier finds
	// "list" which is a CEL builtin (filtered), so deps is empty.
	// checkReadyRef walks ALL .ready() occurrences and finds "d" in
	// readinessDeps only (body = soft dep).
	if deps["d"] {
		t.Error("'d' should not be in deps (ExtractFirstIdentifier finds 'list', which is filtered)")
	}
	if !readinessDeps["d"] {
		t.Error("expected 'd' in readinessDeps (string scanner can't resolve comprehension variables)")
	}
}
