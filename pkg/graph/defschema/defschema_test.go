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

package defschema

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

func isDyn(s *spec.Schema) bool {
	_, ok := s.Extensions["x-kubernetes-int-or-string"]
	return ok
}

func TestInfer(t *testing.T) {
	t.Run("literals take the type of their value", func(t *testing.T) {
		sch, err := Infer(map[string]any{
			"b": true, "i": int64(3), "f": 1.5, "s": "text", "n": nil,
		}, nil, nil)
		require.NoError(t, err)
		assert.True(t, sch.Type.Contains("object"))
		assert.True(t, sch.Properties["b"].Type.Contains("boolean"))
		assert.True(t, sch.Properties["i"].Type.Contains("integer"))
		assert.True(t, sch.Properties["f"].Type.Contains("number"))
		assert.True(t, sch.Properties["s"].Type.Contains("string"))
		n := sch.Properties["n"]
		assert.True(t, isDyn(&n))
	})

	t.Run("expression fields take the recorded output type at their path", func(t *testing.T) {
		sch, err := Infer(
			map[string]any{
				"count": "${schema.spec.n}",
				"nest":  map[string]any{"name": "${schema.spec.name}"},
				"list":  []any{"${schema.spec.n}"},
			},
			map[string]*cel.Type{
				"count":     cel.IntType,
				"nest.name": cel.StringType,
				"list[0]":   cel.IntType,
			}, nil)
		require.NoError(t, err)
		assert.True(t, sch.Properties["count"].Type.Contains("integer"))
		nest := sch.Properties["nest"]
		assert.True(t, nest.Properties["name"].Type.Contains("string"))
		list := sch.Properties["list"]
		assert.True(t, list.Items.Schema.Type.Contains("integer"))
	})

	t.Run("an expression that returns dyn stays dyn", func(t *testing.T) {
		sch, err := Infer(
			map[string]any{"x": "${dyn(1)}"},
			map[string]*cel.Type{"x": cel.DynType}, nil)
		require.NoError(t, err)
		x := sch.Properties["x"]
		assert.True(t, isDyn(&x))
	})

	t.Run("arrays take the element schema, or dyn when empty or mixed", func(t *testing.T) {
		sch, err := Infer(map[string]any{
			"same":  []any{"a", "b"},
			"mixed": []any{"a", true},
			"empty": []any{},
		}, nil, nil)
		require.NoError(t, err)

		same := sch.Properties["same"]
		assert.True(t, same.Items.Schema.Type.Contains("string"))
		mixed := sch.Properties["mixed"]
		assert.True(t, isDyn(mixed.Items.Schema))
		empty := sch.Properties["empty"]
		require.NotNil(t, empty.Items)
		assert.True(t, isDyn(empty.Items.Schema))
	})

	t.Run("an empty object stays a declared field", func(t *testing.T) {
		sch, err := Infer(map[string]any{"empty": map[string]any{}}, nil, nil)
		require.NoError(t, err)
		_, ok := sch.Properties["empty"]
		assert.True(t, ok)
	})

	t.Run("a field name with a dot is addressed by its quoted path", func(t *testing.T) {
		sch, err := Infer(
			map[string]any{"a.b": "${schema.spec.n}"},
			map[string]*cel.Type{`["a.b"]`: cel.IntType}, nil)
		require.NoError(t, err)
		ab := sch.Properties["a.b"]
		assert.True(t, ab.Type.Contains("integer"))
	})
}
