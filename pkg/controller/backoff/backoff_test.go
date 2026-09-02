// Copyright 2025 The Kube Resource Orchestrator Authors
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

package backoff

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func key(ns, name string) client.ObjectKey {
	return client.ObjectKey{Namespace: ns, Name: name}
}

// TestProgression asserts the delay doubles per consecutive attempt starting
// at the seeded base and saturates at Max.
func TestProgression(t *testing.T) {
	b := New(1 * time.Second)
	k := key("test", "typo-loop")

	want := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		32 * time.Second,
		64 * time.Second,
		128 * time.Second,
		256 * time.Second, // 4m16s, still < 5m
		Max,               // 512s would exceed 5m → capped
		Max,               // stays capped
		Max,
	}
	for i, w := range want {
		assert.Equalf(t, w, b.Next(k), "attempt %d", i)
	}
}

// TestSeedsFromConfiguredBase asserts the configured interval is honored as the
// FIRST delay and grows ×Factor from there.
func TestSeedsFromConfiguredBase(t *testing.T) {
	b := New(3 * time.Second)
	k := key("test", "seeded")

	assert.Equal(t, 3*time.Second, b.Next(k), "first attempt is the configured base")
	assert.Equal(t, 6*time.Second, b.Next(k))
	assert.Equal(t, 12*time.Second, b.Next(k))
}

// TestNonPositiveBaseFallsBack asserts a zero/negative base falls back to Base
// rather than degenerating to a 0s hammer.
func TestNonPositiveBaseFallsBack(t *testing.T) {
	k := key("test", "zero")
	assert.Equal(t, Base, New(0).Next(k))
	assert.Equal(t, Base, New(-1*time.Second).Next(k))
}

// TestResetRestartsStreak asserts Reset returns the key to the base delay,
// modeling a clean reconcile after a fixed reference.
func TestResetRestartsStreak(t *testing.T) {
	b := New(1 * time.Second)
	k := key("test", "i")

	assert.Equal(t, 1*time.Second, b.Next(k))
	assert.Equal(t, 2*time.Second, b.Next(k))
	assert.Equal(t, 4*time.Second, b.Next(k))

	b.Reset(k)

	assert.Equal(t, 1*time.Second, b.Next(k), "reset must restart the streak at base")
	assert.Equal(t, 2*time.Second, b.Next(k))
}

// TestPerKeyIsolation asserts one key's streak does not affect another's.
func TestPerKeyIsolation(t *testing.T) {
	b := New(1 * time.Second)
	a := key("test", "a")
	c := key("test", "b")

	assert.Equal(t, 1*time.Second, b.Next(a))
	assert.Equal(t, 2*time.Second, b.Next(a))
	assert.Equal(t, 4*time.Second, b.Next(a))

	assert.Equal(t, 1*time.Second, b.Next(c), "c is independent — first attempt is still base")
	assert.Equal(t, 8*time.Second, b.Next(a), "a keeps climbing from where it was")
}

// TestResetUnknownKeyNoop asserts resetting an untracked key is a no-op and a
// subsequent Next starts fresh.
func TestResetUnknownKeyNoop(t *testing.T) {
	b := New(1 * time.Second)
	k := key("test", "never-seen")
	assert.NotPanics(t, func() { b.Reset(k) })
	assert.Equal(t, 1*time.Second, b.Next(k))
}

// TestNilSafe asserts the nil receiver degrades to a flat Base delay without
// panicking, so a caller that never initialized the tracker still requeues.
func TestNilSafe(t *testing.T) {
	var b *Tracker
	k := key("test", "i")
	assert.NotPanics(t, func() { b.Reset(k) })
	assert.Equal(t, Base, b.Next(k))
	assert.Equal(t, Base, b.Next(k), "nil tracker keeps no state, always Base")
}

// TestConcurrent exercises the tracker from many goroutines to surface data
// races under -race.
func TestConcurrent(t *testing.T) {
	b := New(1 * time.Second)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			k := key("test", string(rune('a'+n%5)))
			for range 100 {
				_ = b.Next(k)
				if n%7 == 0 {
					b.Reset(k)
				}
			}
		}(i)
	}
	wg.Wait()
}
