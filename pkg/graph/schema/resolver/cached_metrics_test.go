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

package resolver

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/kubernetes-sigs/kro/pkg/metrics"
)

// TestCachedSchemaResolver_MetricsInstrumented is the finding #7 regression
// guard: the ResolveSchema path must update the SchemaResolver cache metrics
// that pkg/metrics registers. Counters are process-global, so we assert on
// deltas around a controlled sequence of calls.
func TestCachedSchemaResolver_MetricsInstrumented(t *testing.T) {
	mock := &pushMockResolver{}
	cached, err := NewCachedSchemaResolver(mock, 100)
	require.NoError(t, err)

	widget := gvk("metrics.example.com", "v1", "Widget")

	missBefore := testutil.ToFloat64(metrics.SchemaResolverCacheMissesTotal)
	hitBefore := testutil.ToFloat64(metrics.SchemaResolverCacheHitsTotal)
	evictBefore := testutil.ToFloat64(metrics.SchemaResolverCacheEvictionsTotal)
	errBefore := testutil.ToFloat64(metrics.SchemaResolutionErrorsTotal)

	// First resolve: a miss (delegate called) then observed duration.
	_, err = cached.ResolveSchema(widget)
	require.NoError(t, err)
	// Second resolve: a hit (no delegate call).
	_, err = cached.ResolveSchema(widget)
	require.NoError(t, err)

	assert.InDelta(t, 1, testutil.ToFloat64(metrics.SchemaResolverCacheMissesTotal)-missBefore, 0.0001,
		"first resolve must increment misses once")
	assert.InDelta(t, 1, testutil.ToFloat64(metrics.SchemaResolverCacheHitsTotal)-hitBefore, 0.0001,
		"second resolve must increment hits once")

	// The delegate duration histogram must have observed at least one sample.
	assert.Positive(t, testutil.CollectAndCount(metrics.SchemaResolverAPICallDuration),
		"delegate duration must be observed")

	// Invalidation removes the cached entry, which fires the eviction callback.
	cached.InvalidateGroupKind(widget.GroupKind())
	assert.InDelta(t, 1, testutil.ToFloat64(metrics.SchemaResolverCacheEvictionsTotal)-evictBefore, 0.0001,
		"invalidating a cached GK must count one eviction")

	// No errors on the happy path.
	assert.InDelta(t, 0, testutil.ToFloat64(metrics.SchemaResolutionErrorsTotal)-errBefore, 0.0001,
		"no schema resolution errors on the happy path")
}

// TestCachedSchemaResolver_ErrorMetric verifies the error counter fires when
// the delegate returns an error.
func TestCachedSchemaResolver_ErrorMetric(t *testing.T) {
	mock := &erroringResolver{}
	cached, err := NewCachedSchemaResolver(mock, 10)
	require.NoError(t, err)

	errBefore := testutil.ToFloat64(metrics.SchemaResolutionErrorsTotal)

	_, err = cached.ResolveSchema(gvk("err.example.com", "v1", "Boom"))
	require.Error(t, err)

	assert.InDelta(t, 1, testutil.ToFloat64(metrics.SchemaResolutionErrorsTotal)-errBefore, 0.0001,
		"a delegate error must increment the error counter")
}

// TestCachedSchemaResolver_InvalidateNoopDoesNotCreateEpoch is the finding #6
// guard on the memory footprint: invalidating a GK that was never cached does
// bump its epoch (required to fence any in-flight fetch), but repeated
// invalidation of the SAME never-cached GK reuses one map entry rather than
// leaking a new one per call. It also verifies invalidation of a cached GK
// actually drops the entry so the next resolve re-fetches.
func TestCachedSchemaResolver_InvalidateBoundsAndDrops(t *testing.T) {
	mock := &pushMockResolver{}
	cached, err := NewCachedSchemaResolver(mock, 100)
	require.NoError(t, err)

	never := schema.GroupKind{Group: "never.example.com", Kind: "Ghost"}

	// Many invalidations of the same never-cached GK must not grow the epoch
	// map beyond a single entry for that GK.
	for range 100 {
		cached.InvalidateGroupKind(never)
	}
	cached.mu.RLock()
	entriesForGK := 0
	for gk := range cached.epochs {
		if gk == never {
			entriesForGK++
		}
	}
	cached.mu.RUnlock()
	assert.Equal(t, 1, entriesForGK, "repeated invalidation of one GK must reuse a single epoch entry")

	// Invalidation of a cached GK drops the entry, forcing a re-fetch.
	widget := gvk("drop.example.com", "v1", "Widget")
	_, err = cached.ResolveSchema(widget)
	require.NoError(t, err)
	callsAfterFirst := mock.count()

	cached.InvalidateGroupKind(widget.GroupKind())
	_, err = cached.ResolveSchema(widget)
	require.NoError(t, err)
	assert.Equal(t, callsAfterFirst+1, mock.count(),
		"a resolve after invalidating a cached GK must re-hit the delegate")
}

// erroringResolver always fails.
type erroringResolver struct{}

func (erroringResolver) ResolveSchema(_ schema.GroupVersionKind) (*spec.Schema, error) {
	return nil, errors.New("boom")
}
