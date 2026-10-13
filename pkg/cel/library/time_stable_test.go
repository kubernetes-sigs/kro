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

package library

import (
	"strings"
	"testing"
	"time"

	"github.com/kubernetes-sigs/kro/pkg/cel/sentinels"
)

func stampOf(t *testing.T, v any) sentinels.StableStamp {
	t.Helper()
	s, ok := v.(sentinels.StableStamp)
	if !ok {
		t.Fatalf("expected StableStamp, got %T (%v)", v, v)
	}
	return s
}

func TestStableStringOnce(t *testing.T) {
	env := timeEnv(t)
	now := time.Date(2026, 10, 13, 20, 0, 0, 0, time.UTC)
	got, tv := evalTime(t, env, `time.now().stableString(once())`, now, nil)
	s := stampOf(t, got)
	if s.Keys != nil || !s.Gate || !s.Now.Equal(now) {
		t.Fatalf("unexpected stamp: %+v", s)
	}
	if _, ok := tv.EarliestFlip(); ok {
		t.Fatalf("stableString must not record a requeue flip")
	}
}

func TestStableStringWhenChangedWithGate(t *testing.T) {
	env := timeEnv(t)
	now := time.Date(2026, 10, 13, 20, 0, 0, 0, time.UTC)
	vars := map[string]any{"schema": map[string]any{"version": "10.2", "state": "active", "replicas": int64(3)}}

	got, _ := evalTime(t, env,
		`time.now().stableString(whenChanged(schema.version, schema.replicas).onlyWhen(schema.state == "active"))`, now, vars)
	s := stampOf(t, got)
	if len(s.Keys) != 2 || s.Keys[0] != "10.2" || s.Keys[1] != int64(3) || !s.Gate {
		t.Fatalf("unexpected stamp: %+v", s)
	}

	vars["schema"].(map[string]any)["state"] = "upgrading"
	got, _ = evalTime(t, env,
		`time.now().stableString(whenChanged(schema.version).onlyWhen(schema.state == "active"))`, now, vars)
	if s := stampOf(t, got); s.Gate {
		t.Fatalf("gate should be closed when state != active: %+v", s)
	}

	got, _ = evalTime(t, env,
		`time.now().stableString(whenChanged([schema.version, schema.state]))`, now, vars)
	if s := stampOf(t, got); len(s.Keys) != 2 || s.Keys[1] != "upgrading" {
		t.Fatalf("list form should flatten keys: %+v", s)
	}
}

func TestStableStringRejectsMisuse(t *testing.T) {
	env := timeEnv(t)
	cases := map[string]string{
		// Trigger is argument-only; it has no native rendering.
		`string(once())`: "no matching overload",
		// stableString only exists on kro.Timestamp.
		`"x".stableString(once())`: "no matching overload",
		// Receiver must be anchored to exactly one now(); now()+now() is already
		// rejected by the type checker before the runtime nowCount guard.
		`(time.now() + time.now()).stableString(once())`: "no matching overload for '_+_'",
		// whenChanged needs at least one key.
		`time.now().stableString(whenChanged([]))`: "at least one key",
	}
	for expr, want := range cases {
		err, _ := evalTimeErr(t, env, expr, nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want error containing %q, got %v", expr, want, err)
		}
	}
}

// The receiver is the instant to latch, so an offset from now() is a
// first-class use: a TTL frozen at creation, a backoff deadline, a backdate.
func TestStableStringOffsetReceiver(t *testing.T) {
	env := timeEnv(t)
	now := time.Date(2026, 10, 13, 20, 0, 0, 0, time.UTC)

	got, _ := evalTime(t, env, `(time.now() + duration("24h")).stableString(once())`, now, nil)
	if s := stampOf(t, got); !s.Now.Equal(now.Add(24 * time.Hour)) {
		t.Fatalf("expected now+24h, got %v", s.Now)
	}
	got, _ = evalTime(t, env, `(time.now() - duration("1h")).stableString(once())`, now, nil)
	if s := stampOf(t, got); !s.Now.Equal(now.Add(-time.Hour)) {
		t.Fatalf("expected now-1h, got %v", s.Now)
	}
}

func TestStableStringFormat(t *testing.T) {
	env := timeEnv(t)
	now := time.Date(2026, 10, 13, 20, 0, 0, 0, time.UTC)
	got, _ := evalTime(t, env, `time.now().stableString(once(), "2006-01-02")`, now, nil)
	if s := stampOf(t, got); s.Format != "2006-01-02" {
		t.Fatalf("format not carried: %+v", s)
	}
	got, _ = evalTime(t, env, `time.now().stableString(once())`, now, nil)
	if s := stampOf(t, got); s.Format != "" {
		t.Fatalf("default format must be empty (RFC3339): %+v", s)
	}
}

// KREP example: propagation control. The per-instance slot offset arrives as
// data, so the gate is a plain line in now and the solver requeues at
// start + slot.
func TestKREPExamplePropagationControl(t *testing.T) {
	env := timeEnv(t)
	now := time.Date(2026, 10, 13, 20, 0, 0, 0, time.UTC)
	start := now.Add(-2 * time.Minute).Format(time.RFC3339)
	vars := map[string]any{"schema": map[string]any{"start": start, "app": map[string]any{"slot": "5m"}}}
	got, tv := evalTime(t, env, `time.now() >= timestamp(schema.start) + duration(schema.app.slot)`, now, vars)
	if got != false {
		t.Fatalf("2m into a 5m slot the gate must be closed, got %v", got)
	}
	flip, ok := tv.EarliestFlip()
	if !ok || !flip.Equal(now.Add(3*time.Minute)) {
		t.Fatalf("want flip at start+5m (%v), got %v ok=%v", now.Add(3*time.Minute), flip, ok)
	}
}

// KREP example: certificate renewal guard. Opens 5 days before a 90 day expiry.
func TestKREPExampleCertRenewalGuard(t *testing.T) {
	env := timeEnv(t)
	now := time.Date(2026, 10, 13, 20, 0, 0, 0, time.UTC)
	renewed := now.Add(-80 * 24 * time.Hour).Format(time.RFC3339) // 80 days ago
	vars := map[string]any{"credential": map[string]any{"spec": map[string]any{"renewedTime": renewed}}}
	got, tv := evalTime(t, env,
		`time.now() >= timestamp(credential.spec.renewedTime) + duration("2160h") - duration("120h")`, now, vars)
	if got != false {
		t.Fatalf("80 days in, renewal (opens at day 85) must be closed, got %v", got)
	}
	flip, ok := tv.EarliestFlip()
	if !ok || !flip.Equal(now.Add(5*24*time.Hour)) {
		t.Fatalf("want flip 5 days out, got %v ok=%v", flip, ok)
	}
}
