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
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// conditionKey identifies a condition on a single instance. It deliberately
// excludes status and reason so there is at most one cache entry (one emitted
// series) per condition type per instance: caching a new status/reason
// overwrites the previous one instead of leaving a stale series behind.
type conditionKey struct {
	gvr           string
	namespace     string
	name          string
	conditionType string
}

// conditionValue is the mutable part of a cache entry: the condition's current
// status and reason (emitted as labels) and the LastTransitionTime the
// duration is measured from.
type conditionValue struct {
	status string
	reason string
	ts     time.Time
}

func (k conditionKey) labelValues(v conditionValue) []string {
	return []string{k.gvr, k.namespace, k.name, k.conditionType, v.status, v.reason}
}

// durationCollector computes instance_condition_current_status_seconds at
// scrape time. It caches each condition's LastTransitionTime and returns
// time-since on Collect, so the value stays accurate while the controller
// is idle — unlike a GaugeVec set at reconcile time, which would freeze.
type durationCollector struct {
	desc *prometheus.Desc

	mu      sync.RWMutex
	now     func() time.Time // injectable for tests
	entries map[conditionKey]conditionValue
}

func newDurationCollector(fqName, help string) *durationCollector {
	labelNames := []string{
		labelGVR,
		labelNamespace,
		labelName,
		labelConditionType,
		labelConditionStatus,
		labelReason,
	}
	return &durationCollector{
		desc:    prometheus.NewDesc(fqName, help, labelNames, nil),
		now:     time.Now,
		entries: make(map[conditionKey]conditionValue),
	}
}

func (c *durationCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *durationCollector) Collect(ch chan<- prometheus.Metric) {
	// Snapshot under the lock, then emit unlocked: channel sends can block
	// on a slow scrape consumer and must not stall Cache/Evict.
	c.mu.RLock()
	snapshot := make([]struct {
		key conditionKey
		val conditionValue
	}, 0, len(c.entries))
	for k, v := range c.entries {
		snapshot = append(snapshot, struct {
			key conditionKey
			val conditionValue
		}{k, v})
	}
	c.mu.RUnlock()

	now := c.now()
	for _, e := range snapshot {
		seconds := now.Sub(e.val.ts).Seconds()
		if seconds < 0 {
			seconds = 0 // clamp clock skew / future timestamps
		}
		ch <- prometheus.MustNewConstMetric(
			c.desc,
			prometheus.GaugeValue,
			seconds,
			e.key.labelValues(e.val)...,
		)
	}
}

// Cache records the current status, reason, and transition time for a
// condition, replacing any previous entry for the same condition type so only
// the current series is emitted. A real (non-zero) ts always overwrites. A
// zero ts anchors the series to now on first sighting and is preserved on
// later re-caches of the same (status, reason), so a condition without a
// LastTransitionTime grows steadily rather than resetting each reconcile.
func (c *durationCollector) Cache(key conditionKey, status, reason string, ts time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ts.IsZero() {
		if prev, exists := c.entries[key]; exists && prev.status == status && prev.reason == reason {
			return
		}
		ts = c.now()
	}
	c.entries[key] = conditionValue{status: status, reason: reason, ts: ts}
}

// EvictKey removes the entry for a single condition type on an instance.
func (c *durationCollector) EvictKey(key conditionKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

func (c *durationCollector) EvictInstance(gvr, namespace, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.gvr == gvr && k.namespace == namespace && k.name == name {
			delete(c.entries, k)
		}
	}
}

func (c *durationCollector) EvictGVR(gvr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.gvr == gvr {
			delete(c.entries, k)
		}
	}
}
