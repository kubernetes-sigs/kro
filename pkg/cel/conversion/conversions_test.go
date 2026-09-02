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

	val, _, err := prog.Eval(map[string]any{})
	require.NoError(t, err)

	native, err := GoNativeType(val)
	require.NoError(t, err)

	list, ok := native.([]any)
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

	val, _, err := prog.Eval(map[string]any{})
	require.NoError(t, err)

	native, err := GoNativeType(val)
	require.NoError(t, err)

	// Check type
	list, ok := native.([]any)
	require.True(t, ok, "Expected []interface{}, got %T", native)
	require.Equal(t, 2, len(list))

	// Check element type
	map1, ok := list[0].(map[string]any)
	require.True(t, ok, "Expected map[string]interface{} for element 0, got %T", list[0])
	assert.EqualValues(t, 1, map1["a"])

	map2, ok := list[1].(map[string]any)
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

	val, _, err := prog.Eval(map[string]any{})
	require.NoError(t, err)

	native, err := GoNativeType(val)
	require.NoError(t, err)

	// Check JSON marshalling
	_, err = json.Marshal(native)
	assert.NoError(t, err, "Should be JSON marshallable")
}

func TestGoNativeType_Bytes(t *testing.T) {
	// GoNativeType itself has no JSON-safety restriction: some callers (e.g.
	// json.marshal, via encoding/json) can handle []byte directly. Callers
	// that feed apimachinery's unstructured deep-copy must additionally call
	// EnsureJSONSafe; see TestEnsureJSONSafe_Bytes.
	env, err := cel.NewEnv()
	require.NoError(t, err)

	ast, issues := env.Compile(`b"hello world"`)
	require.NoError(t, issues.Err())

	prog, err := env.Program(ast)
	require.NoError(t, err)

	val, _, err := prog.Eval(map[string]any{})
	require.NoError(t, err)

	native, err := GoNativeType(val)
	require.NoError(t, err)
	assert.Equal(t, []byte("hello world"), native)
}

func TestGoNativeType_Uint(t *testing.T) {
	env, err := cel.NewEnv()
	require.NoError(t, err)

	ast, issues := env.Compile(`uint(42)`)
	require.NoError(t, issues.Err())

	prog, err := env.Program(ast)
	require.NoError(t, err)

	val, _, err := prog.Eval(map[string]any{})
	require.NoError(t, err)

	native, err := GoNativeType(val)
	require.NoError(t, err)

	// GoNativeType returns the raw uint64; JSON-safety normalization (to
	// int64, for callers that need it) is EnsureJSONSafe's job, not this
	// function's - see TestEnsureJSONSafe_Uint.
	u, ok := native.(uint64)
	require.True(t, ok, "Expected uint64, got %T", native)
	assert.Equal(t, uint64(42), u)

	// Check JSON marshalling
	marshalled, err := json.Marshal(native)
	assert.NoError(t, err, "Should be JSON marshallable")
	assert.NotEmpty(t, marshalled)
}

func TestEnsureJSONSafe_Bytes(t *testing.T) {
	// Raw bytes have no JSON-safe representation (apimachinery's unstructured
	// deep-copy panics on []byte), so EnsureJSONSafe must reject them with an
	// actionable error instead of returning a value that later panics.
	_, err := EnsureJSONSafe([]byte("hello world"))
	require.ErrorIs(t, err, ErrUnsupportedType)
}

func TestEnsureJSONSafe_Uint(t *testing.T) {
	safe, err := EnsureJSONSafe(uint64(42))
	require.NoError(t, err)

	// EnsureJSONSafe converts uint64 to int64 so it survives apimachinery's
	// unstructured deep-copy, which only accepts int64 (not uint64).
	i, ok := safe.(int64)
	require.True(t, ok, "Expected int64, got %T", safe)
	assert.Equal(t, int64(42), i)

	marshalled, err := json.Marshal(safe)
	assert.NoError(t, err, "Should be JSON marshallable")
	assert.NotEmpty(t, marshalled)
}

func TestEnsureJSONSafe_UintOverflow(t *testing.T) {
	_, err := EnsureJSONSafe(uint64(18446744073709551615)) // math.MaxUint64
	require.ErrorIs(t, err, ErrUnsupportedType)
}

func TestEnsureJSONSafe_NestedList(t *testing.T) {
	safe, err := EnsureJSONSafe([]any{uint64(42), "ok"})
	require.NoError(t, err)
	assert.Equal(t, []any{int64(42), "ok"}, safe)

	_, err = EnsureJSONSafe([]any{[]byte("bad")})
	require.ErrorIs(t, err, ErrUnsupportedType)
}

func TestEnsureJSONSafe_NestedMap(t *testing.T) {
	safe, err := EnsureJSONSafe(map[string]any{"n": uint64(42)})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"n": int64(42)}, safe)

	_, err = EnsureJSONSafe(map[string]any{"b": []byte("bad")})
	require.ErrorIs(t, err, ErrUnsupportedType)
}

func TestConvertMap_DeepCopiesRawMap(t *testing.T) {
	// When the underlying CEL value wraps a raw map[string]interface{},
	// convertMap should return a deep copy so that mutations to the
	// result do not affect the original.
	original := map[string]any{
		"key": "value",
		"nested": map[string]any{
			"inner": "data",
		},
		"list": []any{"a", "b"},
	}

	// Wrap the raw map as a CEL ref.Val via the default type adapter.
	reg := types.NewEmptyRegistry()
	celVal := reg.NativeToValue(original)
	require.Equal(t, types.MapType, celVal.Type())

	result, err := GoNativeType(celVal)
	require.NoError(t, err)

	resultMap, ok := result.(map[string]any)
	require.True(t, ok, "Expected map[string]interface{}, got %T", result)

	// The values should be equal.
	assert.Equal(t, original, resultMap)

	// Mutate the result and verify the original is unchanged.
	resultMap["key"] = "mutated"
	assert.Equal(t, "value", original["key"], "Original should not be affected by mutation of result")

	nestedResult, ok := resultMap["nested"].(map[string]any)
	require.True(t, ok)
	nestedResult["inner"] = "mutated"
	nestedOriginal := original["nested"].(map[string]any)
	assert.Equal(t, "data", nestedOriginal["inner"], "Original nested map should not be affected by mutation of result")
}

func TestGoNativeType_Duration(t *testing.T) {
	env, err := cel.NewEnv()
	require.NoError(t, err)

	ast, issues := env.Compile(`duration("1h30m")`)
	require.NoError(t, issues.Err())

	prog, err := env.Program(ast)
	require.NoError(t, err)

	val, _, err := prog.Eval(map[string]any{})
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

	val, _, err := prog.Eval(map[string]any{})
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
