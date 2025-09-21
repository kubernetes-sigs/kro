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

	"golang.org/x/sync/singleflight"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// CachedEmulator wraps an Emulator with caching functionality
type CachedEmulator struct {
	emulator Interface

	mu    sync.RWMutex
	cache map[schema.GroupVersionKind]*unstructured.Unstructured

	group singleflight.Group
}

// NewCachedEmulator creates a new CachedEmulator wrapping the standard Emulator
func NewCachedEmulator() *CachedEmulator {
	return &CachedEmulator{
		emulator: NewEmulator(),
		cache:    make(map[schema.GroupVersionKind]*unstructured.Unstructured),
	}
}

// CachedFromEmulator creates a new CachedEmulator wrapping the provided Emulator
func CachedFromEmulator(emulator Interface) *CachedEmulator {
	return &CachedEmulator{
		emulator: emulator,
		cache:    make(map[schema.GroupVersionKind]*unstructured.Unstructured),
	}
}

// GenerateSample generates a dummy CR, returning cached version if available
func (c *CachedEmulator) GenerateSample(gvk schema.GroupVersionKind, schema *spec.Schema) (*unstructured.Unstructured, error) {
	// singleflight ensures only one goroutine generates per key
	result, err, _ := c.group.Do(gvk.String(), func() (interface{}, error) {
		c.mu.RLock()
		// check cache inside singleflight
		if cached, ok := c.cache[gvk]; ok {
			c.mu.RUnlock()
			return cached, nil
		}
		c.mu.RUnlock()

		// generate new CR
		cr, err := c.emulator.GenerateSample(gvk, schema)
		if err != nil {
			return nil, err
		}

		// Store in cache
		c.mu.Lock()
		c.cache[gvk] = cr
		c.mu.Unlock()

		return cr, nil
	})

	if err != nil {
		return nil, err
	}

	return result.(*unstructured.Unstructured), nil
}
