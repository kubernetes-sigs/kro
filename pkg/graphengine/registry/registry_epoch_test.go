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

package registry

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
)

// TestRegistry_DeletePrunesEpoch is a regression test for a memory leak:
// Delete used to drop the entry from the main store but leave the per-key
// epoch behind, so the epochs map grew without bound under Graph churn
// (Len()==0 while len(epochs) tracked every Graph ever compiled). Delete
// must prune the epoch entry too, keeping epochs bounded to live keys.
func TestRegistry_DeletePrunesEpoch(t *testing.T) {
	t.Parallel()

	t.Run("delete prunes the epoch for the removed key", func(t *testing.T) {
		t.Parallel()
		r := New()
		r.Store(key("a"), "h", &compiler.Program{})
		r.Store(key("b"), "h", &compiler.Program{})

		r.Delete(key("a"))

		assert.Equal(t, 1, r.Len(), "store should retain only the live key")
		assert.NotContains(t, r.epochs, key("a"), "deleted key must be pruned from epochs")
		assert.Contains(t, r.epochs, key("b"), "surviving key must keep its epoch")
		assert.Len(t, r.epochs, r.Len(), "epochs must stay bounded to live entries")
	})

	t.Run("epochs does not leak under churn", func(t *testing.T) {
		t.Parallel()
		r := New()
		const n = 1000
		for i := 0; i < n; i++ {
			k := key(string(rune('A') + rune(i%26))) // reuse a small key space
			k.Name = k.Name + string(rune('0')+rune(i%10)) + itoa(i)
			r.Store(k, "h", &compiler.Program{})
			r.Delete(k)
		}
		assert.Equal(t, 0, r.Len(), "store should be empty after deleting every key")
		assert.Empty(t, r.epochs, "epochs must not retain deleted keys (leak regression)")
	})

	t.Run("re-created key after delete gets a fresh epoch", func(t *testing.T) {
		t.Parallel()
		r := New()
		r.Store(key("g"), "h", &compiler.Program{})
		firstEpoch := r.epochs[key("g")]

		r.Delete(key("g"))
		assert.NotContains(t, r.epochs, key("g"), "epoch pruned on delete")

		r.Store(key("g"), "h2", &compiler.Program{})
		secondEpoch := r.epochs[key("g")]
		assert.Greater(t, secondEpoch, firstEpoch,
			"re-created key must get a strictly larger, monotonic epoch")

		got, hit := r.Lookup(key("g"), "h2")
		assert.True(t, hit, "re-created entry should be live")
		assert.NotNil(t, got)
		_, staleHit := r.Lookup(key("g"), "h")
		assert.False(t, staleHit, "old hash must not resolve after recreate")
	})
}

// itoa is a tiny base-10 formatter kept local to this test to avoid pulling
// in strconv just for unique key generation.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
