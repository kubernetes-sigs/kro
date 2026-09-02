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
	"reflect"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kubernetes-sigs/kro/pkg/cel/sentinels"
)

// The ref.Val protocol methods on kro's custom CEL values are called by cel-go
// itself, not by kro, whenever an author's expression compares, converts, or
// otherwise handles one of these values. They are easy to leave untested
// precisely because nothing in kro calls them directly.

func testCondition() *Condition {
	return &Condition{
		ConditionType: "Ready",
		Status:        "True",
		Reason:        "ResourcesReady",
		Message:       "all resources applied",
	}
}

func TestConditionConvertToNative(t *testing.T) {
	t.Parallel()
	c := testCondition()

	t.Run("a map target yields the wire field names", func(t *testing.T) {
		t.Parallel()
		out, err := c.ConvertToNative(reflect.TypeFor[map[string]any]())
		require.NoError(t, err)
		assert.Equal(t, map[string]any{
			"type":    "Ready",
			"status":  "True",
			"reason":  "ResourcesReady",
			"message": "all resources applied",
		}, out, "the map keys are the wire condition field names, not the Go field names")
	})

	t.Run("a pointer target yields the same instance", func(t *testing.T) {
		t.Parallel()
		out, err := c.ConvertToNative(reflect.TypeFor[*Condition]())
		require.NoError(t, err)
		assert.Same(t, c, out)
	})

	t.Run("a value target yields a copy", func(t *testing.T) {
		t.Parallel()
		out, err := c.ConvertToNative(reflect.TypeFor[Condition]())
		require.NoError(t, err)
		copied, ok := out.(Condition)
		require.True(t, ok)
		assert.Equal(t, *c, copied)
	})

	t.Run("an unsupported target is an error, not a panic", func(t *testing.T) {
		t.Parallel()
		_, err := c.ConvertToNative(reflect.TypeFor[string]())
		require.Error(t, err)
		assert.Contains(t, err.Error(), ConditionTypeName)
	})
}

func TestConditionConvertToType(t *testing.T) {
	t.Parallel()
	c := testCondition()

	assert.Same(t, c, c.ConvertToType(c.Type()),
		"converting to its own type returns the value unchanged")

	asType := c.ConvertToType(types.TypeType)
	require.NotNil(t, asType)
	assert.Equal(t, ConditionTypeName, asType.(*cel.Type).TypeName(),
		"converting to type yields the Condition type itself")

	assert.True(t, types.IsError(c.ConvertToType(types.StringType)),
		"an unsupported conversion is a CEL error value")
}

func TestConditionEqual(t *testing.T) {
	t.Parallel()

	assert.Equal(t, types.True, testCondition().Equal(testCondition()),
		"conditions with identical fields are equal")

	other := testCondition()
	other.Status = "False"
	assert.Equal(t, types.False, testCondition().Equal(other),
		"a differing field makes them unequal")

	other = testCondition()
	other.Message = "different"
	assert.Equal(t, types.False, testCondition().Equal(other),
		"equality covers message, not just type and status")

	assert.True(t, types.IsError(testCondition().Equal(types.String("Ready"))),
		"comparing a Condition to a non-Condition is a no-such-overload error")
}

func TestConditionTypeAndValue(t *testing.T) {
	t.Parallel()
	c := testCondition()
	assert.Equal(t, ConditionTypeName, c.Type().TypeName())
	assert.Same(t, c, c.Value())
}

// Get backs dot-notation field access in author expressions (cond.status).
func TestConditionGet(t *testing.T) {
	t.Parallel()
	c := testCondition()

	for key, want := range map[string]string{
		"type":    "Ready",
		"status":  "True",
		"reason":  "ResourcesReady",
		"message": "all resources applied",
	} {
		got := c.Get(types.String(key))
		assert.Equal(t, types.String(want), got, "cond.%s", key)
	}

	assert.True(t, types.IsError(c.Get(types.String("nope"))),
		"an unknown field is a CEL error naming the field")
	assert.True(t, types.IsError(c.Get(types.Int(0))),
		"a non-string key is a CEL error rather than a panic")
}

func TestRuntimeSingletonRefVal(t *testing.T) {
	t.Parallel()

	_, err := RuntimeSingleton.ConvertToNative(reflect.TypeFor[map[string]any]())
	require.Error(t, err, "the runtime singleton has no native representation")
	assert.Contains(t, err.Error(), RuntimeTypeName)

	assert.Same(t, RuntimeSingleton, RuntimeSingleton.ConvertToType(RuntimeSingleton.Type()),
		"converting to its own type returns the singleton")

	asType := RuntimeSingleton.ConvertToType(types.TypeType)
	assert.Equal(t, RuntimeTypeName, asType.(*cel.Type).TypeName())

	assert.True(t, types.IsError(RuntimeSingleton.ConvertToType(types.StringType)))

	assert.Equal(t, types.True, RuntimeSingleton.Equal(RuntimeSingleton),
		"the singleton equals itself")
	assert.Equal(t, types.False, RuntimeSingleton.Equal(types.String("runtime")),
		"and is not equal to an unrelated value")

	assert.Equal(t, RuntimeTypeName, RuntimeSingleton.Type().TypeName())
	assert.NotNil(t, RuntimeSingleton.Value())
}

// omit() is a field-removal sentinel, not a value. It deliberately refuses
// comparison: if `omit() == omit()` returned true, an author could branch on it
// and the resolver's remove-this-field contract would leak into expression
// semantics.
func TestOmitValRefVal(t *testing.T) {
	t.Parallel()

	t.Run("it converts to the sentinel struct", func(t *testing.T) {
		t.Parallel()
		out, err := omitInstance.ConvertToNative(reflect.TypeFor[sentinels.Omit]())
		require.NoError(t, err)
		assert.Equal(t, sentinels.Omit{}, out)
	})

	t.Run("an interface target also yields the sentinel", func(t *testing.T) {
		t.Parallel()
		var iface any
		out, err := omitInstance.ConvertToNative(reflect.TypeOf(&iface).Elem())
		require.NoError(t, err)
		assert.Equal(t, sentinels.Omit{}, out)
	})

	t.Run("any other native target is an error", func(t *testing.T) {
		t.Parallel()
		_, err := omitInstance.ConvertToNative(reflect.TypeFor[string]())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "omit")
	})

	t.Run("it converts to its type but not to others", func(t *testing.T) {
		t.Parallel()
		asType := omitInstance.ConvertToType(types.TypeType)
		require.NotNil(t, asType)
		assert.True(t, types.IsError(omitInstance.ConvertToType(types.StringType)))
	})

	t.Run("it refuses comparison", func(t *testing.T) {
		t.Parallel()
		assert.True(t, types.IsError(omitInstance.Equal(omitInstance)),
			"omit() must not be comparable, even to itself")
		assert.True(t, types.IsError(omitInstance.Equal(types.String("omit"))))
	})

	t.Run("its type and value are the sentinel", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "kro.omit", omitInstance.Type().TypeName())
		assert.Equal(t, sentinels.Omit{}, omitInstance.Value())
	})
}

// IsConditionType is how callers decide whether a checked expression's output
// is a Condition, which gates author-condition handling.
func TestIsConditionType(t *testing.T) {
	t.Parallel()

	assert.False(t, IsConditionType(nil), "a nil type must not be reported as a Condition")
	assert.True(t, IsConditionType(conditionType))
	assert.False(t, IsConditionType(cel.StringType))
	assert.False(t, IsConditionType(runtimeType),
		"the runtime singleton's type is not a Condition type")
}
