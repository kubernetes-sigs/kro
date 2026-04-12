// cache_test.go contains regression tests for content-addressed apply gating
// and section-scoped hashing. Each test targets a specific correctness
// invariant that the implementation must maintain.
package graphcontroller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Absent path sentinel regression tests
//
// Regression: hashNodeInputs and hashSelfSections previously used `continue`
// when a referenced section was absent from the observed data. This meant
// absent→present transitions (e.g., an upstream resource gaining a `status`
// block for the first time) produced the same hash as before, so downstream
// nodes never re-evaluated.
//
// Fix: absent sections now hash a fixed sentinel value. The transition from
// absent to present changes the hash, triggering re-evaluation.
// ---------------------------------------------------------------------------

// TestHashNodeInputsAbsentSentinel proves that hashNodeInputs produces
// different hashes when a referenced section transitions from absent to
// present. Without the sentinel, both cases hash identically.
func TestHashNodeInputsAbsentSentinel(t *testing.T) {
	node := &Node{
		ID: "child",
		DepSections: map[string]map[string]bool{
			"parent": {"status": true, "spec": true},
		},
	}

	// Scope where parent has spec but NOT status (status absent).
	scopeAbsent := map[string]any{
		"parent": map[string]any{
			"spec": map[string]any{"replicas": int64(3)},
			// "status" is absent
		},
	}

	// Scope where parent has both spec AND status (status present).
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
		"absent→present section transition must change the hash; "+
			"without the sentinel, downstream nodes would never re-evaluate")
}

// TestHashNodeInputsAbsentStable proves that the absent sentinel is
// deterministic — hashing the same absent state twice produces identical
// results. This prevents spurious re-evaluations.
func TestHashNodeInputsAbsentStable(t *testing.T) {
	node := &Node{
		ID: "child",
		DepSections: map[string]map[string]bool{
			"parent": {"status": true},
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

// TestHashSelfSectionsAbsentSentinel proves that hashSelfSections produces
// different hashes when a self-referenced section transitions from absent to
// present. This triggers gate re-evaluation (readyWhen/propagateWhen) when
// the resource gains a new section.
func TestHashSelfSectionsAbsentSentinel(t *testing.T) {
	node := &Node{
		ID:           "deploy",
		SelfSections: map[string]bool{"status": true},
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

	hashAbsent, err := hashSelfSections(node, observedAbsent)
	require.NoError(t, err)
	require.NotEmpty(t, hashAbsent)

	hashPresent, err := hashSelfSections(node, observedPresent)
	require.NoError(t, err)
	require.NotEmpty(t, hashPresent)

	assert.NotEqual(t, hashAbsent, hashPresent,
		"absent→present self section transition must change the hash; "+
			"without the sentinel, gate conditions would never re-evaluate")
}

// TestHashSelfSectionsAbsentStable proves the absent sentinel for
// hashSelfSections is deterministic.
func TestHashSelfSectionsAbsentStable(t *testing.T) {
	node := &Node{
		ID:           "deploy",
		SelfSections: map[string]bool{"status": true},
	}

	observed := map[string]any{
		"spec": map[string]any{"replicas": int64(3)},
		// "status" is absent
	}

	hash1, err := hashSelfSections(node, observed)
	require.NoError(t, err)

	hash2, err := hashSelfSections(node, observed)
	require.NoError(t, err)

	assert.Equal(t, hash1, hash2,
		"hashing the same absent self section must produce identical results")
}

// ---------------------------------------------------------------------------
// Resource cache regression tests
//
// Regression: applyContribution cached the template hash after the main
// resource patch succeeded. If the subsequent status subresource patch
// failed, the cache still held the hash — next reconcile saw a match and
// skipped the apply entirely. The resource was silently diverged.
//
// Fix: on status subresource failure, Resources.remove(cacheKey) is called
// before returning the error. The next reconcile sees a cache miss and
// retries both patches.
// ---------------------------------------------------------------------------

// TestResourceCacheRemoveForcesReapply proves that removing a cache entry
// causes subsequent gets to return a miss, which forces re-apply on the
// next reconcile. This is the mechanism the status-fail-revert fix relies on.
func TestResourceCacheRemoveForcesReapply(t *testing.T) {
	rc := newResourceCache()
	key := "v1/ConfigMap/default/test"

	// Simulate a successful apply that populates the cache.
	rc.set(key, &cachedObject{
		resourceVersion: "1000",
		templateHash:    "abc123",
		object:          map[string]any{"kind": "ConfigMap"},
	})

	// Cache hit — normal steady-state behavior.
	cached, ok := rc.get(key)
	require.True(t, ok, "cache should hit after set")
	assert.Equal(t, "abc123", cached.templateHash)

	// Simulate the status-fail fix: remove the entry.
	rc.remove(key)

	// Cache miss — next reconcile will re-apply.
	_, ok = rc.get(key)
	assert.False(t, ok,
		"cache must miss after remove; without this, status subresource "+
			"failures would leave the resource silently diverged")
}
