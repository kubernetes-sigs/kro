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

package metrics

import (
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	io_prometheus_client "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestCollector(now time.Time) *durationCollector {
	c := newDurationCollector("test_metric", "test help")
	c.now = func() time.Time { return now }
	return c
}

func (c *durationCollector) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[conditionKey]conditionValue)
}

func (c *durationCollector) size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

func keyFor(name string) conditionKey {
	return conditionKey{
		gvr:           "kro.run/v1alpha1, Resource=tests",
		namespace:     "default",
		name:          name,
		conditionType: "Ready",
	}
}

func TestDurationCollector_CacheAndCollect(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 30, 0, time.UTC)
	c := newTestCollector(now)

	ts := now.Add(-10 * time.Second)
	c.Cache(keyFor("a"), "True", "AllReady", ts)

	got := collectorEntries(t, c)
	require.Len(t, got, 1)
	assert.InDelta(t, 10.0, got[0].value, 0.001,
		"value should be (now - cachedTimestamp) in seconds")
	assert.Equal(t, "True", got[0].labels["condition_status"])
	assert.Equal(t, "AllReady", got[0].labels["reason"])
}

func TestDurationCollector_ZeroTimestampTreatedAsNow(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c := newTestCollector(now)

	c.Cache(keyFor("a"), "True", "AllReady", time.Time{})

	got := collectorEntries(t, c)
	require.Len(t, got, 1)
	assert.Equal(t, 0.0, got[0].value,
		"zero timestamp must produce a zero-second initial duration")
}

func TestDurationCollector_ZeroTimestampDoesNotResetOnRecache(t *testing.T) {
	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c := newDurationCollector("test_metric", "test help")
	c.now = func() time.Time { return clock }

	c.Cache(keyFor("a"), "True", "AllReady", time.Time{})

	clock = clock.Add(30 * time.Second)
	c.Cache(keyFor("a"), "True", "AllReady", time.Time{})

	got := collectorEntries(t, c)
	require.Len(t, got, 1)
	assert.Equal(t, 30.0, got[0].value,
		"a re-cached zero timestamp must keep growing, not reset the anchor")
}

func TestDurationCollector_NegativeDurationClamped(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c := newTestCollector(now)

	future := now.Add(5 * time.Second)
	c.Cache(keyFor("a"), "True", "AllReady", future)

	got := collectorEntries(t, c)
	require.Len(t, got, 1)
	assert.Equal(t, 0.0, got[0].value, "future timestamps must clamp to 0, not emit negative durations")
}

func TestDurationCollector_CacheOverwritesStatusAndReason(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c := newTestCollector(now)

	c.Cache(keyFor("a"), "False", "NotReady", now.Add(-100*time.Second))
	c.Cache(keyFor("a"), "True", "AllReady", now.Add(-5*time.Second))

	got := collectorEntries(t, c)
	require.Len(t, got, 1, "a transition must overwrite, not leave a stale series")
	assert.Equal(t, "True", got[0].labels["condition_status"])
	assert.Equal(t, "AllReady", got[0].labels["reason"])
	assert.InDelta(t, 5.0, got[0].value, 0.001)
}

func TestDurationCollector_EvictKey(t *testing.T) {
	c := newTestCollector(time.Now())
	c.Cache(keyFor("a"), "True", "AllReady", time.Now())
	c.Cache(keyFor("b"), "True", "AllReady", time.Now())

	c.EvictKey(keyFor("a"))

	got := collectorEntries(t, c)
	require.Len(t, got, 1, "only key 'b' should remain")
}

func TestDurationCollector_EvictInstance(t *testing.T) {
	c := newTestCollector(time.Now())
	now := time.Now()

	c.Cache(keyFor("a"), "True", "AllReady", now)
	c.Cache(keyFor("b"), "True", "AllReady", now)

	c.EvictInstance("kro.run/v1alpha1, Resource=tests", "default", "a")

	got := collectorEntries(t, c)
	require.Len(t, got, 1)
	for _, e := range got {
		assert.Equal(t, "b", e.labels["name"])
	}
}

func TestDurationCollector_EvictGVR(t *testing.T) {
	c := newTestCollector(time.Now())
	now := time.Now()

	c.Cache(keyFor("a"), "True", "AllReady", now)
	c.Cache(keyFor("b"), "True", "AllReady", now)

	other := keyFor("c")
	other.gvr = "different.group/v1, Resource=others"
	c.Cache(other, "True", "AllReady", now)

	c.EvictGVR("kro.run/v1alpha1, Resource=tests")

	got := collectorEntries(t, c)
	require.Len(t, got, 1)
	assert.Equal(t, "different.group/v1, Resource=others", got[0].labels["gvr"])
}

func TestDurationCollector_DescribeEmitsDescriptor(t *testing.T) {
	c := newTestCollector(time.Now())

	ch := make(chan *prometheus.Desc, 4)
	c.Describe(ch)
	close(ch)

	descs := []*prometheus.Desc{}
	for d := range ch {
		descs = append(descs, d)
	}
	require.Len(t, descs, 1, "Describe must emit exactly one descriptor")
}

func TestDurationCollector_ScrapeIsConcurrencySafe(t *testing.T) {
	c := newTestCollector(time.Now())

	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				status := []string{"True", "False", "Unknown"}[i%3]
				c.Cache(keyFor("name"), status, "reason", time.Now())
				i++
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = collectorEntries(t, c)
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

type emittedEntry struct {
	value  float64
	labels map[string]string
}

func collectorEntries(t *testing.T, c prometheus.Collector) []emittedEntry {
	t.Helper()
	ch := make(chan prometheus.Metric, 100)
	c.Collect(ch)
	close(ch)

	out := []emittedEntry{}
	for m := range ch {
		var dto io_prometheus_client.Metric
		require.NoError(t, m.Write(&dto))
		labels := make(map[string]string, len(dto.Label))
		for _, lp := range dto.Label {
			labels[lp.GetName()] = lp.GetValue()
		}
		out = append(out, emittedEntry{
			value:  dto.GetGauge().GetValue(),
			labels: labels,
		})
	}
	return out
}
