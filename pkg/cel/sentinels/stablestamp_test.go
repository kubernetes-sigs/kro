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

package sentinels

import (
	"testing"
	"time"
)

var (
	t0 = time.Date(2026, 10, 13, 20, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Hour)
	t2 = t0.Add(2 * time.Hour)
)

func desiredWith(stamp StableStamp) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": "db"},
		"status":   map[string]any{"lastUpgradedTime": stamp},
	}
}

func stampAt(obj map[string]any) (string, bool) {
	v, ok := obj["status"].(map[string]any)["lastUpgradedTime"]
	s, isStr := v.(string)
	return s, ok && isStr
}

func anno(obj map[string]any) (string, bool) {
	return liveAnnotation(obj, StableStampAnnotationKey("status.lastUpgradedTime"))
}

// simulate applies desired as the new live object (what SSA would produce).
func simulate(desired map[string]any) map[string]any { return desired }

func TestStableStampLifecycle(t *testing.T) {
	// 1. create at v10.1, active -> stamps t0.
	d := desiredWith(StableStamp{Keys: []any{"10.1"}, Gate: true, Now: t0})
	ResolveStableStamps(d, nil)
	if s, _ := stampAt(d); s != t0.Format(time.RFC3339) {
		t.Fatalf("create: want %s got %q", t0.Format(time.RFC3339), s)
	}
	if a, ok := anno(d); !ok || a != `["10.1"]` {
		t.Fatalf("create: annotation want [\"10.1\"], got %q %v", a, ok)
	}
	live := simulate(d)

	// 2. no-op reconcile at t1: same keys -> keeps t0.
	d = desiredWith(StableStamp{Keys: []any{"10.1"}, Gate: true, Now: t1})
	ResolveStableStamps(d, live)
	if s, _ := stampAt(d); s != t0.Format(time.RFC3339) {
		t.Fatalf("no-op: must keep t0, got %q", s)
	}
	live = simulate(d)

	// 3. upgrading: version flipped to 10.2 but gate closed -> freeze BOTH.
	d = desiredWith(StableStamp{Keys: []any{"10.2"}, Gate: false, Now: t1})
	ResolveStableStamps(d, live)
	if s, _ := stampAt(d); s != t0.Format(time.RFC3339) {
		t.Fatalf("gated: must keep t0, got %q", s)
	}
	if a, _ := anno(d); a != `["10.1"]` {
		t.Fatalf("gated: annotation must stay at 10.1 (else the transition is swallowed), got %q", a)
	}
	live = simulate(d)

	// 4. active again at 10.2 -> keys differ from baseline -> stamps t2.
	d = desiredWith(StableStamp{Keys: []any{"10.2"}, Gate: true, Now: t2})
	ResolveStableStamps(d, live)
	if s, _ := stampAt(d); s != t2.Format(time.RFC3339) {
		t.Fatalf("upgrade landed: want %s got %q", t2.Format(time.RFC3339), s)
	}
	if a, _ := anno(d); a != `["10.2"]` {
		t.Fatalf("upgrade landed: annotation want [\"10.2\"], got %q", a)
	}
}

func TestStableStampGatedOnCreateWritesNothing(t *testing.T) {
	d := desiredWith(StableStamp{Keys: []any{"10.1"}, Gate: false, Now: t0})
	ResolveStableStamps(d, nil)
	if _, ok := d["status"].(map[string]any)["lastUpgradedTime"]; ok {
		t.Fatalf("gated create must omit the field")
	}
	if _, ok := anno(d); ok {
		t.Fatalf("gated create must not write the annotation")
	}
}

func TestStableStampOnce(t *testing.T) {
	d := desiredWith(StableStamp{Keys: nil, Gate: true, Now: t0})
	ResolveStableStamps(d, nil)
	if s, _ := stampAt(d); s != t0.Format(time.RFC3339) {
		t.Fatalf("once create: got %q", s)
	}
	live := simulate(d)
	d = desiredWith(StableStamp{Keys: nil, Gate: true, Now: t2})
	ResolveStableStamps(d, live)
	if s, _ := stampAt(d); s != t0.Format(time.RFC3339) {
		t.Fatalf("once must never restamp, got %q", s)
	}
}

func TestStableStampOnceRespectsPreexistingValue(t *testing.T) {
	// Adoption: field already set by someone else, no kro annotation yet.
	live := map[string]any{
		"metadata": map[string]any{"name": "db"},
		"status":   map[string]any{"lastUpgradedTime": "2020-01-01T00:00:00Z"},
	}
	d := desiredWith(StableStamp{Keys: nil, Gate: true, Now: t0})
	ResolveStableStamps(d, live)
	if s, _ := stampAt(d); s != "2020-01-01T00:00:00Z" {
		t.Fatalf("once must respect an existing value, got %q", s)
	}
	if _, ok := anno(d); !ok {
		t.Fatalf("once adoption must record a baseline annotation")
	}
}

func TestStableStampNoChangeIsByteIdentical(t *testing.T) {
	d := desiredWith(StableStamp{Keys: []any{"a", int64(1)}, Gate: true, Now: t0})
	ResolveStableStamps(d, nil)
	live := simulate(d)
	again := desiredWith(StableStamp{Keys: []any{"a", int64(1)}, Gate: true, Now: t2})
	ResolveStableStamps(again, live)
	if s1, _ := stampAt(d); s1 != mustStamp(again) {
		t.Fatalf("steady state must be byte-identical")
	}
	a1, _ := anno(d)
	a2, _ := anno(again)
	if a1 != a2 {
		t.Fatalf("annotation must be byte-identical: %q vs %q", a1, a2)
	}
}

func mustStamp(obj map[string]any) string {
	s, _ := stampAt(obj)
	return s
}
