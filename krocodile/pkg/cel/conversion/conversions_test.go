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

package conversion

import (
	"encoding/json"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGoNativeType_EmptyList(t *testing.T) {
	env, err := cel.NewEnv()
	require.NoError(t, err)

	ast, issues := env.Compile(`[]`)
	require.NoError(t, issues.Err())

	prog, err := env.Program(ast)
	require.NoError(t, err)

	val, _, err := prog.Eval(map[string]interface{}{})
	require.NoError(t, err)

	native, err := GoNativeType(val)
	require.NoError(t, err)

	list, ok := native.([]interface{})
	require.True(t, ok, "Expected []interface{}, got %T", native)
	assert.NotNil(t, list)
	assert.Equal(t, 0, len(list))
}

func TestGoNativeType_ListMap(t *testing.T) {
	env, err := cel.NewEnv()
	require.NoError(t, err)

	ast, issues := env.Compile(`[{"a": 1}, {"b": 2}]`)
	require.NoError(t, issues.Err())

	prog, err := env.Program(ast)
	require.NoError(t, err)

	val, _, err := prog.Eval(map[string]interface{}{})
	require.NoError(t, err)

	native, err := GoNativeType(val)
	require.NoError(t, err)

	// Check type
	list, ok := native.([]interface{})
	require.True(t, ok, "Expected []interface{}, got %T", native)
	require.Equal(t, 2, len(list))

	// Check element type
	map1, ok := list[0].(map[string]interface{})
	require.True(t, ok, "Expected map[string]interface{} for element 0, got %T", list[0])
	assert.EqualValues(t, 1, map1["a"])

	map2, ok := list[1].(map[string]interface{})
	require.True(t, ok, "Expected map[string]interface{} for element 1, got %T", list[1])
	assert.EqualValues(t, 2, map2["b"])

	// Check JSON marshalling
	_, err = json.Marshal(native)
	assert.NoError(t, err, "Should be JSON marshallable")
}

func TestGoNativeType_ComplexNested(t *testing.T) {
	env, err := cel.NewEnv()
	require.NoError(t, err)

	// List of maps with list values
	expr := `[
		{"name": "foo", "items": ["a", "b"]},
		{"name": "bar", "items": ["c"]}
	]`
	ast, issues := env.Compile(expr)
	require.NoError(t, issues.Err())

	prog, err := env.Program(ast)
	require.NoError(t, err)

	val, _, err := prog.Eval(map[string]interface{}{})
	require.NoError(t, err)

	native, err := GoNativeType(val)
	require.NoError(t, err)

	// Check JSON marshalling
	_, err = json.Marshal(native)
	assert.NoError(t, err, "Should be JSON marshallable")
}

func TestGoNativeType_Bytes(t *testing.T) {
	env, err := cel.NewEnv()
	require.NoError(t, err)

	ast, issues := env.Compile(`b"hello world"`)
	require.NoError(t, issues.Err())

	prog, err := env.Program(ast)
	require.NoError(t, err)

	val, _, err := prog.Eval(map[string]interface{}{})
	require.NoError(t, err)

	native, err := GoNativeType(val)
	require.NoError(t, err)

	// Check type
	bytes, ok := native.([]byte)
	require.True(t, ok, "Expected []byte, got %T", native)
	assert.Equal(t, []byte("hello world"), bytes)

	// Check JSON marshalling
	marshalled, err := json.Marshal(native)
	assert.NoError(t, err, "Should be JSON marshallable")
	assert.NotEmpty(t, marshalled)
}

func TestConvertMap_DeepCopiesRawMap(t *testing.T) {
	// When the underlying CEL value wraps a raw map[string]interface{},
	// convertMap should return a deep copy so that mutations to the
	// result do not affect the original.
	original := map[string]interface{}{
		"key": "value",
		"nested": map[string]interface{}{
			"inner": "data",
		},
		"list": []interface{}{"a", "b"},
	}

	// Wrap the raw map as a CEL ref.Val via the default type adapter.
	reg := types.NewEmptyRegistry()
	celVal := reg.NativeToValue(original)
	require.Equal(t, types.MapType, celVal.Type())

	result, err := GoNativeType(celVal)
	require.NoError(t, err)

	resultMap, ok := result.(map[string]interface{})
	require.True(t, ok, "Expected map[string]interface{}, got %T", result)

	// The values should be equal.
	assert.Equal(t, original, resultMap)

	// Mutate the result and verify the original is unchanged.
	resultMap["key"] = "mutated"
	assert.Equal(t, "value", original["key"], "Original should not be affected by mutation of result")

	nestedResult, ok := resultMap["nested"].(map[string]interface{})
	require.True(t, ok)
	nestedResult["inner"] = "mutated"
	nestedOriginal := original["nested"].(map[string]interface{})
	assert.Equal(t, "data", nestedOriginal["inner"], "Original nested map should not be affected by mutation of result")
}

func TestGoNativeType_Duration(t *testing.T) {
	env, err := cel.NewEnv()
	require.NoError(t, err)

	ast, issues := env.Compile(`duration("1h30m")`)
	require.NoError(t, issues.Err())

	prog, err := env.Program(ast)
	require.NoError(t, err)

	val, _, err := prog.Eval(map[string]interface{}{})
	require.NoError(t, err)

	native, err := GoNativeType(val)
	require.NoError(t, err)

	// GoNativeType converts durations to strings for JSON-safe unstructured objects.
	str, ok := native.(string)
	require.True(t, ok, "Expected string, got %T", native)
	assert.Equal(t, "1h30m0s", str)

	// Check JSON marshalling
	marshalled, err := json.Marshal(native)
	assert.NoError(t, err, "Should be JSON marshallable")
	assert.NotEmpty(t, marshalled)
}

func TestGoNativeType_Timestamp(t *testing.T) {
	env, err := cel.NewEnv()
	require.NoError(t, err)

	// Test timestamp conversion using RFC3339 format
	ast, issues := env.Compile(`timestamp("2024-01-15T10:30:00Z")`)
	require.NoError(t, issues.Err())

	prog, err := env.Program(ast)
	require.NoError(t, err)

	val, _, err := prog.Eval(map[string]interface{}{})
	require.NoError(t, err)

	native, err := GoNativeType(val)
	require.NoError(t, err)

	// GoNativeType converts timestamps to RFC3339 strings for JSON-safe unstructured objects.
	str, ok := native.(string)
	require.True(t, ok, "Expected string, got %T", native)
	assert.Equal(t, "2024-01-15T10:30:00Z", str)

	// Check JSON marshalling
	marshalled, err := json.Marshal(native)
	assert.NoError(t, err, "Should be JSON marshallable")
	assert.NotEmpty(t, marshalled)
}
