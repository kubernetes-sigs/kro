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

package instance

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
)

func TestResolveExternalCollectionSelector(t *testing.T) {
	configMap := func(selector interface{}) *unstructured.Unstructured {
		metadata := map[string]interface{}{
			"name":      "configs",
			"namespace": "default",
		}
		if selector != nil {
			metadata["selector"] = selector
		}
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   metadata,
		}}
	}

	t.Run("malformed selectors fail closed", func(t *testing.T) {
		tests := []struct {
			name     string
			selector interface{}
		}{
			{
				name:     "bare label map without matchLabels",
				selector: map[string]interface{}{"app": "demo"},
			},
			{
				name:     "misspelled matchLabels key",
				selector: map[string]interface{}{"matchLabel": map[string]interface{}{"app": "demo"}},
			},
			{
				name: "unknown field in matchExpressions item",
				selector: map[string]interface{}{
					"matchExpressions": []interface{}{
						map[string]interface{}{"key": "app", "operator": "In", "values": []interface{}{"demo"}, "bogus": true},
					},
				},
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				selector, err := resolveExternalCollectionSelector("configs", configMap(tt.selector))
				require.Error(t, err,
					"selector %#v resolved to %#v, which lists every ConfigMap in scope",
					tt.selector, selector)
			})
		}
	})

	t.Run("absent or empty selectors select everything", func(t *testing.T) {
		tests := []struct {
			name     string
			selector interface{}
		}{
			{name: "no selector field", selector: nil},
			{name: "empty selector object", selector: map[string]interface{}{}},
			{name: "empty matchLabels", selector: map[string]interface{}{"matchLabels": map[string]interface{}{}}},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				selector, err := resolveExternalCollectionSelector("configs", configMap(tt.selector))
				require.NoError(t, err)
				assert.Equal(t, labels.Everything(), selector)
			})
		}
	})

	t.Run("valid selector", func(t *testing.T) {
		selector, err := resolveExternalCollectionSelector("configs", configMap(map[string]interface{}{
			"matchLabels": map[string]interface{}{"app": "demo"},
		}))
		require.NoError(t, err)
		assert.Equal(t, "app=demo", selector.String())
	})
}
