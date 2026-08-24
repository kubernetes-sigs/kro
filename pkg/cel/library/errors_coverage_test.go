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
	"testing"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every kro CEL function rejects bad arguments with a message naming the
// function, because that message is what an RGD author sees when their
// expression is wrong — a bare "no such overload" from cel-go would leave them
// guessing which call failed.

// assertCELErr asserts that val is a CEL error whose message contains want.
func assertCELErr(t *testing.T, val ref.Val, want string) {
	t.Helper()
	require.True(t, types.IsError(val), "expected a CEL error, got %v", val)
	assert.Contains(t, val.(*types.Err).String(), want)
}

func TestHashRejectsNonStringArguments(t *testing.T) {
	t.Parallel()
	cases := map[string]func(ref.Val) ref.Val{
		"hash.fnv64a": fnv64aHash,
		"hash.sha256": sha256Hash,
		"hash.md5":    md5Hash,
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assertCELErr(t, fn(types.Bool(true)), name)
			assertCELErr(t, fn(types.NewRefValList(
				types.DefaultTypeAdapter, []ref.Val{types.String("a")})), name)
		})
	}
}

func TestJSONErrorPaths(t *testing.T) {
	t.Parallel()

	t.Run("unmarshal rejects a non-string argument", func(t *testing.T) {
		t.Parallel()
		assertCELErr(t, unmarshalJSON(types.Bool(true)), "json.unmarshal")
	})

	t.Run("unmarshal reports the parse failure", func(t *testing.T) {
		t.Parallel()
		assertCELErr(t, unmarshalJSON(types.String(`{"broken":`)), "failed to parse")
	})

	t.Run("unmarshal accepts valid JSON", func(t *testing.T) {
		t.Parallel()
		out := unmarshalJSON(types.String(`{"a":1}`))
		require.False(t, types.IsError(out))
	})

	t.Run("marshal round-trips a list", func(t *testing.T) {
		t.Parallel()
		out := marshalJSON(types.NewRefValList(
			types.DefaultTypeAdapter, []ref.Val{types.Int(1), types.Int(2)}))
		require.False(t, types.IsError(out), "got %v", out)
		assert.Equal(t, types.String("[1,2]"), out)
	})
}

func TestMergeRejectsNonMaps(t *testing.T) {
	t.Parallel()
	m := types.NewStringStringMap(types.DefaultTypeAdapter, map[string]string{"a": "1"})

	assertCELErr(t, mergeVals(types.String("not-a-map"), m), "no such overload")
	assertCELErr(t, mergeVals(m, types.String("not-a-map")), "no such overload")
}

func TestSeededIntArgumentValidation(t *testing.T) {
	t.Parallel()
	seed := types.String("s")

	assertCELErr(t, generateDeterministicInt(types.Int(0)), "exactly 3 arguments")
	assertCELErr(t,
		generateDeterministicInt(types.String("x"), types.Int(9), seed), "min must be an integer")
	assertCELErr(t,
		generateDeterministicInt(types.Int(0), types.String("x"), seed), "max must be an integer")
	assertCELErr(t,
		generateDeterministicInt(types.Int(0), types.Int(9), types.Int(1)), "seed must be a string")
	assertCELErr(t,
		generateDeterministicInt(types.Int(9), types.Int(9), seed), "min must be less than max")
	assertCELErr(t,
		generateDeterministicInt(types.Int(10), types.Int(9), seed), "min must be less than max")

	// The same seed and bounds must always produce the same value; a seeded
	// generator that drifted would make every dependent resource churn.
	first := generateDeterministicInt(types.Int(0), types.Int(100), seed)
	second := generateDeterministicInt(types.Int(0), types.Int(100), seed)
	require.False(t, types.IsError(first), "got %v", first)
	assert.Equal(t, first, second, "the same seed must yield the same value")
}

func TestSeededStringArgumentValidation(t *testing.T) {
	t.Parallel()
	seed := types.String("s")

	assertCELErr(t, generateDeterministicString(types.String("8"), seed), "length must be an integer")
	assertCELErr(t, generateDeterministicString(types.Int(0), seed), "length must be positive")
	assertCELErr(t, generateDeterministicString(types.Int(-1), seed), "length must be positive")
	assertCELErr(t, generateDeterministicString(types.Int(8), types.Int(1)), "seed must be a string")

	first := generateDeterministicString(types.Int(8), seed)
	require.False(t, types.IsError(first), "got %v", first)
	assert.Equal(t, first, generateDeterministicString(types.Int(8), seed),
		"the same seed must yield the same string")

	// Longer than one sha256 digest can index, to exercise the rehash path.
	long := generateDeterministicString(types.Int(64), seed)
	require.False(t, types.IsError(long), "got %v", long)
	assert.Len(t, string(long.(types.String)), 64)
}

func TestListIndexFunctionValidation(t *testing.T) {
	t.Parallel()
	list := types.NewRefValList(types.DefaultTypeAdapter,
		[]ref.Val{types.String("a"), types.String("b")})

	t.Run("setAtIndex", func(t *testing.T) {
		t.Parallel()
		assertCELErr(t, listsSetAtIndex(list, types.Int(0)), "expected 3 arguments")
		assertCELErr(t, listsSetAtIndex(types.String("x"), types.Int(0), types.String("v")),
			"must be a list")
		assertCELErr(t, listsSetAtIndex(list, types.String("0"), types.String("v")),
			"index must be an integer")
		assertCELErr(t, listsSetAtIndex(list, types.Int(-1), types.String("v")), "out of bounds")
		assertCELErr(t, listsSetAtIndex(list, types.Int(2), types.String("v")), "out of bounds")
	})

	t.Run("insertAtIndex", func(t *testing.T) {
		t.Parallel()
		assertCELErr(t, listsInsertAtIndex(list, types.Int(0)), "expected 3 arguments")
		assertCELErr(t, listsInsertAtIndex(types.String("x"), types.Int(0), types.String("v")),
			"must be a list")
		assertCELErr(t, listsInsertAtIndex(list, types.String("0"), types.String("v")),
			"index must be an integer")
		assertCELErr(t, listsInsertAtIndex(list, types.Int(-1), types.String("v")), "out of bounds")
		assertCELErr(t, listsInsertAtIndex(list, types.Int(3), types.String("v")), "out of bounds")

		// Inserting at size() appends rather than erroring, unlike setAtIndex.
		out := listsInsertAtIndex(list, types.Int(2), types.String("c"))
		require.False(t, types.IsError(out), "got %v", out)
		assert.Equal(t, types.Int(3), out.(traits.Lister).Size())
	})

	t.Run("removeAtIndex", func(t *testing.T) {
		t.Parallel()
		assertCELErr(t, listsRemoveAtIndex(types.String("x"), types.Int(0)), "must be a list")
		assertCELErr(t, listsRemoveAtIndex(list, types.String("0")), "index must be an integer")
		assertCELErr(t, listsRemoveAtIndex(list, types.Int(-1)), "out of bounds")
		assertCELErr(t, listsRemoveAtIndex(list, types.Int(2)), "out of bounds")
	})
}

// The version options select which function set a library exposes, so an
// environment pinned to an older version keeps compiling expressions written
// against it.
func TestLibraryVersionOptions(t *testing.T) {
	t.Parallel()

	lists := &listsLibrary{}
	assert.Equal(t, uint32(3), ListsVersion(3)(lists).version)

	maps := &mapsLib{}
	assert.Equal(t, uint32(2), MapsVersion(2)(maps).version)
}

// The Condition type is surfaced to CEL through a custom type provider, so the
// provider has to report its field names and types and delegate everything else
// to the wrapped provider.
func TestConditionTypeProviderFieldNames(t *testing.T) {
	t.Parallel()

	p := &conditionTypeProvider{Provider: types.NewEmptyRegistry()}

	names, found := p.FindStructFieldNames(ConditionTypeName)
	require.True(t, found)
	assert.ElementsMatch(t,
		[]string{"type", "status", "reason", "message"}, names,
		"the provider must report the wire field names authors write")

	_, found = p.FindStructFieldNames("some.other.Type")
	assert.False(t, found, "unknown types must fall through to the wrapped provider")

	ft, found := p.FindStructFieldType(ConditionTypeName, "status")
	require.True(t, found)
	assert.Equal(t, types.StringType, ft.Type)

	_, found = p.FindStructFieldType(ConditionTypeName, "nope")
	assert.False(t, found, "an unknown Condition field must not resolve")
}
