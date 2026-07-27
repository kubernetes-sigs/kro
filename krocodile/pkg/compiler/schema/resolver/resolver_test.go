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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// errResolver always returns the configured error, counting calls so the
// delegate-error rows can assert the cache never memoizes a failure.
type errResolver struct {
	err   error
	calls int
}

func (e *errResolver) ResolveSchema(_ schema.GroupVersionKind) (*spec.Schema, error) {
	e.calls++
	return nil, e.err
}

// TestNewCachedSchemaResolver covers the constructor's success and failure
// surfaces in one table: a positive size builds a usable resolver, while a
// non-positive size bubbles up the LRU's error.
func TestNewCachedSchemaResolver(t *testing.T) {
	tests := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "positive-size-succeeds", size: 10, wantErr: false},
		{name: "zero-size-errors", size: 0, wantErr: true},
		{name: "negative-size-errors", size: -1, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewCachedSchemaResolver(&mockResolver{}, tc.size)
			if tc.wantErr {
				require.Error(t, err)
				assert.Nil(t, c)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, c)
		})
	}
}

// TestCachedSchemaResolver_ResolveSchema_DelegateError pins the error path:
// a failing delegate must surface its error and must NOT be memoized, so a
// subsequent call re-invokes the delegate.
func TestCachedSchemaResolver_ResolveSchema_DelegateError(t *testing.T) {
	tests := []struct {
		name      string
		delegate  *errResolver
		wantCalls int
	}{
		{
			name:      "error-propagates-and-is-not-cached",
			delegate:  &errResolver{err: errors.New("boom")},
			wantCalls: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewCachedSchemaResolver(tc.delegate, 10)
			require.NoError(t, err)

			k := gvk("apps", "v1", "Deployment")

			sch, err := c.ResolveSchema(k)
			require.Error(t, err)
			assert.Nil(t, sch)

			// Error results are not memoized, so a second call re-hits the delegate.
			sch, err = c.ResolveSchema(k)
			require.Error(t, err)
			assert.Nil(t, sch)

			assert.Equal(t, tc.wantCalls, tc.delegate.calls)
		})
	}
}

// TestNewCombinedResolver exercises the wiring helper. Building a discovery
// client from a rest.Config is offline (no server contact), so a minimal
// config yields a combined resolver plus the live cached resolver for
// invalidation plumbing.
func TestNewCombinedResolver(t *testing.T) {
	tests := []struct {
		name    string
		config  *rest.Config
		wantErr bool
	}{
		{
			name:    "minimal-config-wires-resolvers",
			config:  &rest.Config{Host: "https://example.invalid"},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			combined, cached, err := NewCombinedResolver(tc.config, nil)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, combined)
			assert.NotNil(t, cached)
		})
	}
}
