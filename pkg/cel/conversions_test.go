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

package cel

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	// Check type and value
	duration, ok := native.(time.Duration)
	require.True(t, ok, "Expected time.Duration, got %T", native)
	assert.Equal(t, time.Hour+30*time.Minute, duration)

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

	// Check type
	ts, ok := native.(time.Time)
	require.True(t, ok, "Expected time.Time, got %T", native)

	// Verify the timestamp is correct
	expected, _ := time.Parse(time.RFC3339, "2024-01-15T10:30:00Z")
	assert.Equal(t, expected, ts)

	// Check JSON marshalling
	marshalled, err := json.Marshal(native)
	assert.NoError(t, err, "Should be JSON marshallable")
	assert.NotEmpty(t, marshalled)
}
