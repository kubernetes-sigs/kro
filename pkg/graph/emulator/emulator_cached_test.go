// Copyright 2025 The Kubernetes Authors.
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

package emulator

import (
	"sync"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// mockEmulator implements Interface and counts calls
type mockEmulator struct {
	callCount atomic.Int32
}

func (m *mockEmulator) GenerateSample(gvk schema.GroupVersionKind, schema *spec.Schema) (*unstructured.Unstructured, error) {
	m.callCount.Add(1)

	// Return a simple sample
	cr := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"spec": "test-value",
		},
	}
	cr.SetAPIVersion(gvk.GroupVersion().String())
	cr.SetKind(gvk.Kind)
	cr.SetName("test-sample")

	return cr, nil
}

func TestCachedEmulator_OnlyGeneratesOnce(t *testing.T) {
	mock := &mockEmulator{}
	ce := &CachedEmulator{
		emulator: mock,
		cache:    make(map[schema.GroupVersionKind]*unstructured.Unstructured),
	}

	gvk := schema.GroupVersionKind{
		Group:   "apps",
		Version: "v1",
		Kind:    "Deployment",
	}

	// Launch 100 concurrent requests
	var wg sync.WaitGroup
	results := make([]*unstructured.Unstructured, 100)

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cr, err := ce.GenerateSample(gvk, nil)
			if err != nil {
				t.Errorf("goroutine %d: unexpected error: %v", i, err)
				return
			}
			results[i] = cr
		}()
	}

	wg.Wait()

	// Check only called once
	if count := mock.callCount.Load(); count != 1 {
		t.Errorf("expected 1 call to GenerateSample, got %d", count)
	}

	// Verify all goroutines got a result
	for i, result := range results {
		if result == nil {
			t.Errorf("goroutine %d got nil result", i)
		}
	}
}
