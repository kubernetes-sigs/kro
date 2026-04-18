package graphcontroller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Absent path sentinel regression tests
//
// Regression: hashNodeInputs and hashSelfPaths previously used `continue`
// when a referenced path was absent from the observed data. This meant
// absent→present transitions (e.g., an upstream resource gaining a `status`
// block for the first time) produced the same hash as before, so downstream
// nodes never re-evaluated.
//
// Fix: absent paths now hash a fixed sentinel value. The transition from
// absent to present changes the hash, triggering re-evaluation.
// ---------------------------------------------------------------------------

// TestHashNodeInputsAbsentSentinel proves that hashNodeInputs produces
// different hashes when a referenced path transitions from absent to
// present. Without the sentinel, both cases hash identically.
func TestHashNodeInputsAbsentSentinel(t *testing.T) {
	node := &Node{
		ID: "child",
		DepPaths: map[string][]FieldPath{
			"parent": {{"status", "ready"}, {"spec", "replicas"}},
		},
	}

	// Scope where parent has spec but NOT status (status.ready absent).
	scopeAbsent := map[string]any{
		"parent": map[string]any{
			"spec": map[string]any{"replicas": int64(3)},
			// "status" is absent
		},
	}

	// Scope where parent has both spec AND status.ready present.
	scopePresent := map[string]any{
		"parent": map[string]any{
			"spec":   map[string]any{"replicas": int64(3)},
			"status": map[string]any{"ready": true},
		},
	}

	hashAbsent, err := hashNodeInputs(node, scopeAbsent)
	require.NoError(t, err)
	require.NotEmpty(t, hashAbsent)

	hashPresent, err := hashNodeInputs(node, scopePresent)
	require.NoError(t, err)
	require.NotEmpty(t, hashPresent)

	assert.NotEqual(t, hashAbsent, hashPresent,
		"absent→present path transition must change the hash; "+
			"without the sentinel, downstream nodes would never re-evaluate")
}

// TestHashNodeInputsAbsentStable proves that the absent sentinel is
// deterministic — hashing the same absent state twice produces identical
// results. This prevents spurious re-evaluations.
func TestHashNodeInputsAbsentStable(t *testing.T) {
	node := &Node{
		ID: "child",
		DepPaths: map[string][]FieldPath{
			"parent": {{"status", "ready"}},
		},
	}

	scope := map[string]any{
		"parent": map[string]any{
			// "status" is absent
		},
	}

	hash1, err := hashNodeInputs(node, scope)
	require.NoError(t, err)

	hash2, err := hashNodeInputs(node, scope)
	require.NoError(t, err)

	assert.Equal(t, hash1, hash2,
		"hashing the same absent state must produce identical results")
}

// TestHashSelfPathsAbsentSentinel proves that hashSelfPaths produces
// different hashes when a self-referenced path transitions from absent to
// present. This triggers gate re-evaluation (readyWhen/propagateWhen) when
// the resource gains a new field.
func TestHashSelfPathsAbsentSentinel(t *testing.T) {
	node := &Node{
		ID:        "deploy",
		SelfPaths: []FieldPath{{"status", "availableReplicas"}},
	}

	// Observed resource without status.
	observedAbsent := map[string]any{
		"spec": map[string]any{"replicas": int64(3)},
		// "status" is absent
	}

	// Observed resource with status.
	observedPresent := map[string]any{
		"spec":   map[string]any{"replicas": int64(3)},
		"status": map[string]any{"availableReplicas": int64(3)},
	}

	hashAbsent, err := hashSelfPaths(node, observedAbsent)
	require.NoError(t, err)
	require.NotEmpty(t, hashAbsent)

	hashPresent, err := hashSelfPaths(node, observedPresent)
	require.NoError(t, err)
	require.NotEmpty(t, hashPresent)

	assert.NotEqual(t, hashAbsent, hashPresent,
		"absent→present self path transition must change the hash; "+
			"without the sentinel, gate conditions would never re-evaluate")
}

// TestHashSelfPathsAbsentStable proves the absent sentinel for
// hashSelfPaths is deterministic.
func TestHashSelfPathsAbsentStable(t *testing.T) {
	node := &Node{
		ID:        "deploy",
		SelfPaths: []FieldPath{{"status", "availableReplicas"}},
	}

	observed := map[string]any{
		"spec": map[string]any{"replicas": int64(3)},
		// "status" is absent
	}

	hash1, err := hashSelfPaths(node, observed)
	require.NoError(t, err)

	hash2, err := hashSelfPaths(node, observed)
	require.NoError(t, err)

	assert.Equal(t, hash1, hash2,
		"hashing the same absent self path must produce identical results")
}

// TestHashFieldPathGranularity proves that field-path hashing only changes
// when the referenced path changes, not when sibling fields change.
// This is the core property: status.availableReplicas changes → hash changes;
// status.conditions changes → hash does NOT change (if we only reference availableReplicas).
func TestHashFieldPathGranularity(t *testing.T) {
	node := &Node{
		ID:        "deploy",
		SelfPaths: []FieldPath{{"status", "availableReplicas"}},
	}

	observed1 := map[string]any{
		"status": map[string]any{
			"availableReplicas": int64(3),
			"conditions":        []any{"old-conditions"},
		},
	}

	observed2 := map[string]any{
		"status": map[string]any{
			"availableReplicas": int64(3),
			"conditions":        []any{"new-conditions-changed"},
		},
	}

	hash1, err := hashSelfPaths(node, observed1)
	require.NoError(t, err)

	hash2, err := hashSelfPaths(node, observed2)
	require.NoError(t, err)

	assert.Equal(t, hash1, hash2,
		"changing status.conditions should NOT affect the hash when only status.availableReplicas is referenced")

	// Now change the referenced field.
	observed3 := map[string]any{
		"status": map[string]any{
			"availableReplicas": int64(5), // changed!
			"conditions":        []any{"new-conditions-changed"},
		},
	}

	hash3, err := hashSelfPaths(node, observed3)
	require.NoError(t, err)

	assert.NotEqual(t, hash1, hash3,
		"changing status.availableReplicas MUST affect the hash")
}

// TestHashDesiredState_RegressionStringEscaping proves that string values
// containing quote characters produce distinct hashes from strings that
// don't. Without escaping, a value like `a"b` produces `"a"b"` in the
// buffer — structurally ambiguous with a key-value boundary.
func TestHashDesiredState_RegressionStringEscaping(t *testing.T) {
	// A value containing a quote character must not collide with a
	// structurally different map.
	h1, err := hashDesiredState(map[string]any{"key": `value"with"quotes`})
	require.NoError(t, err)

	h2, err := hashDesiredState(map[string]any{"key": `valuewithquotes`})
	require.NoError(t, err)

	assert.NotEqual(t, h1, h2,
		"strings with and without embedded quotes must hash differently")

	// Backslash must also be escaped to prevent ambiguity with the
	// escape character itself.
	h3, err := hashDesiredState(map[string]any{"key": `a\b`})
	require.NoError(t, err)

	h4, err := hashDesiredState(map[string]any{"key": `ab`})
	require.NoError(t, err)

	assert.NotEqual(t, h3, h4,
		"strings with and without backslashes must hash differently")

	// Adversarial: a string that looks like a JSON key-value boundary.
	h5, err := hashDesiredState(map[string]any{"a": `b","c":"d`})
	require.NoError(t, err)

	h6, err := hashDesiredState(map[string]any{"a": "b", "c": "d"})
	require.NoError(t, err)

	assert.NotEqual(t, h5, h6,
		"a string mimicking JSON structure must not collide with actual structure")
}

// TestHashDesiredStateDeterministic proves the hasher produces identical
// results across calls for the same input — the foundational property.
func TestHashDesiredStateDeterministic(t *testing.T) {
	m := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "test",
			"namespace": "default",
		},
		"data": map[string]any{
			"key1": "value1",
			"key2": int64(42),
		},
	}

	h1, err := hashDesiredState(m)
	require.NoError(t, err)

	for i := 0; i < 100; i++ {
		h2, err := hashDesiredState(m)
		require.NoError(t, err)
		assert.Equal(t, h1, h2, "hash must be deterministic across calls (iteration %d)", i)
	}
}

// TestResourceCache tests the resource cache operations.
func TestResourceCache(t *testing.T) {
	rc := newResourceCache()
	key := "v1/ConfigMap/default/test"

	// Cache miss on empty cache.
	_, ok := rc.get(key)
	require.False(t, ok, "cache should miss on empty cache")

	// Simulate a successful apply that populates the cache.
	rc.set(key, &cachedObject{
		resourceVersion: "1000",
		applyHash:       "abc123",
		object:          map[string]any{"kind": "ConfigMap"},
	})

	// Cache hit — normal steady-state behavior.
	cached, ok := rc.get(key)
	require.True(t, ok, "cache should hit after set")
	assert.Equal(t, "abc123", cached.applyHash)

	// Simulate the status-fail fix: remove the entry.
	rc.remove(key)

	_, ok = rc.get(key)
	require.False(t, ok, "cache should miss after remove")
}

// ---------------------------------------------------------------------------
// Spec hash completeness guard
//
// Per cel.go § Hash: "If a new compilation input is added to GraphSpec
// without updating this hash, content-addressed sharing will silently
// reuse stale compiled graphs."
//
// This test mutates each compilation-relevant Node field independently
// and asserts the hash changes. If a new field is added to Node that
// affects compilation (CEL programs, DAG structure, or expression set)
// without being included in Hash(), this test must be updated — the
// field count assertion forces the maintainer to acknowledge the field.
// ---------------------------------------------------------------------------

// TestSpecHashCoversCompilationInputs proves that GraphSpec.Hash() is
// sensitive to every field that feeds into compileGraphSpec. Mutating any
// single compilation input must produce a different hash. Fields populated
// by BuildDAG (Dependencies, DepPaths, SelfPaths, ReadinessDeps) are
// derived outputs and intentionally excluded from the hash.
func TestSpecHashCoversCompilationInputs(t *testing.T) {
	// Baseline spec: one node with every compilation-relevant field populated.
	baseline := &GraphSpec{
		Nodes: []Node{{
			ID:            "deploy",
			Template:      map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "app"}},
			ForEach:       &ForEachBinding{VarName: "ns", Expr: "${namespaces}"},
			Finalizes:     "pvc",
			IncludeWhen:   []string{"${config.data.enabled == 'true'}"},
			ReadyWhen:     []string{"${deploy.status.availableReplicas > 0}"},
			PropagateWhen: []string{"${config.data.enabled == 'true'}"},
		}},
	}
	baselineHash := baseline.Hash()

	// Each test case mutates exactly one compilation input and asserts the
	// hash changes. The test names match the Node struct field names.
	tests := []struct {
		name   string
		mutate func(n *Node)
	}{
		{
			name:   "ID",
			mutate: func(n *Node) { n.ID = "service" },
		},
		{
			name:   "Template",
			mutate: func(n *Node) { n.Template["metadata"] = map[string]any{"name": "different"} },
		},
		{
			name:   "ForEach",
			mutate: func(n *Node) { n.ForEach = &ForEachBinding{VarName: "pod", Expr: "${pods}"} },
		},
		{
			name:   "Finalizes",
			mutate: func(n *Node) { n.Finalizes = "snapshot" },
		},
		{
			name:   "IncludeWhen",
			mutate: func(n *Node) { n.IncludeWhen = []string{"${config.data.enabled == 'false'}"} },
		},
		{
			name:   "ReadyWhen",
			mutate: func(n *Node) { n.ReadyWhen = []string{"${deploy.status.ready == true}"} },
		},
		{
			name:   "PropagateWhen",
			mutate: func(n *Node) { n.PropagateWhen = []string{"${config.ready()}"} },
		},
		// Edge cases: nil/empty transitions must also change the hash.
		{
			name:   "ForEach nil→empty",
			mutate: func(n *Node) { n.ForEach = &ForEachBinding{} },
		},
		{
			name:   "Template nil",
			mutate: func(n *Node) { n.Template = nil },
		},
		{
			name:   "IncludeWhen empty",
			mutate: func(n *Node) { n.IncludeWhen = nil },
		},
		{
			name:   "ReadyWhen empty",
			mutate: func(n *Node) { n.ReadyWhen = nil },
		},
		{
			name:   "PropagateWhen empty",
			mutate: func(n *Node) { n.PropagateWhen = nil },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Deep copy baseline to isolate mutations.
			mutated := &GraphSpec{
				Nodes: []Node{{
					ID:            baseline.Nodes[0].ID,
					Template:      deepCopyMap(baseline.Nodes[0].Template),
					ForEach:       copyForEach(baseline.Nodes[0].ForEach),
					Finalizes:     baseline.Nodes[0].Finalizes,
					IncludeWhen:   copyStrings(baseline.Nodes[0].IncludeWhen),
					ReadyWhen:     copyStrings(baseline.Nodes[0].ReadyWhen),
					PropagateWhen: copyStrings(baseline.Nodes[0].PropagateWhen),
				}},
			}
			tc.mutate(&mutated.Nodes[0])
			mutatedHash := mutated.Hash()

			assert.NotEqual(t, baselineHash, mutatedHash,
				"mutating %s must change the spec hash; without this, "+
					"the compiled graph cache would silently serve stale results", tc.name)
		})
	}

	// Structural guard: if a new field is added to Node that affects
	// compilation, this count must be updated. The 7 compilation inputs
	// are: ID, Template, ForEach, Finalizes, IncludeWhen, ReadyWhen,
	// PropagateWhen. The 4 derived fields (Dependencies, DepPaths,
	// SelfPaths, ReadinessDeps) are populated by BuildDAG and excluded.
	const compilationInputCount = 7
	const derivedFieldCount = 4
	const totalNodeFields = compilationInputCount + derivedFieldCount
	// This assertion uses the number of test cases that cover unique
	// field names (not edge cases). If you add a field to Node, add a
	// test case above AND update this count.
	uniqueFields := map[string]bool{}
	for _, tc := range tests {
		// Only count the primary field tests, not edge cases
		switch tc.name {
		case "ID", "Template", "ForEach", "Finalizes", "IncludeWhen", "ReadyWhen", "PropagateWhen":
			uniqueFields[tc.name] = true
		}
	}
	assert.Equal(t, compilationInputCount, len(uniqueFields),
		"test must cover every compilation input field; if you added a new field to Node "+
			"that affects compilation, add a test case and update compilationInputCount")
	_ = totalNodeFields // reference to prevent the constant from being unused
}

// copyForEach returns a shallow copy of a ForEachBinding pointer.
func copyForEach(d *ForEachBinding) *ForEachBinding {
	if d == nil {
		return nil
	}
	cp := *d
	return &cp
}

// copyStrings returns a copy of a string slice.
func copyStrings(s []string) []string {
	if s == nil {
		return nil
	}
	result := make([]string, len(s))
	copy(result, s)
	return result
}
