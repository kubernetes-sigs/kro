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
