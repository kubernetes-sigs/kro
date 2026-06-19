/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package unstructured

import (
	"reflect"
	"testing"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// mapListSchemaWithKeyCount builds a map-list schema with N keys named k0..kN-1.
func mapListSchemaWithKeyCount(n int) *openapi.Schema {
	props := map[string]spec.Schema{}
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		name := "k" + string(rune('0'+i))
		keys[i] = name
		props[name] = spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}}
	}
	props["val"] = spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}}
	return mapListSchema(props, keys)
}

func elem(keyCount int, vals ...string) map[string]interface{} {
	m := map[string]interface{}{"val": "v"}
	for i := 0; i < keyCount; i++ {
		m["k"+string(rune('0'+i))] = vals[i]
	}
	return m
}

// TestUnstructuredMapList_KeyCounts exercises the 1, 2, 3, and N key-prop
// branches of toMapKey via map-list equality (which builds the key map).
func TestUnstructuredMapList_KeyCounts(t *testing.T) {
	cases := []struct {
		name     string
		keyCount int
		a        []interface{}
		b        []interface{}
		want     ref.Val
	}{
		{
			name:     "one key",
			keyCount: 1,
			a:        []interface{}{elem(1, "x"), elem(1, "y")},
			b:        []interface{}{elem(1, "y"), elem(1, "x")},
			want:     types.True,
		},
		{
			name:     "two keys",
			keyCount: 2,
			a:        []interface{}{elem(2, "a", "b"), elem(2, "c", "d")},
			b:        []interface{}{elem(2, "c", "d"), elem(2, "a", "b")},
			want:     types.True,
		},
		{
			name:     "three keys",
			keyCount: 3,
			a:        []interface{}{elem(3, "a", "b", "c")},
			b:        []interface{}{elem(3, "a", "b", "c")},
			want:     types.True,
		},
		{
			name:     "four keys serialized fallback",
			keyCount: 4,
			a:        []interface{}{elem(4, "a", "b", "c", "d")},
			b:        []interface{}{elem(4, "a", "b", "c", "d")},
			want:     types.True,
		},
		{
			name:     "two keys mismatch",
			keyCount: 2,
			a:        []interface{}{elem(2, "a", "b")},
			b:        []interface{}{elem(2, "x", "y")},
			want:     types.False,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mapListSchemaWithKeyCount(tc.keyCount)
			a := UnstructuredToVal(tc.a, s)
			b := UnstructuredToVal(tc.b, s)
			assert.Equal(t, tc.want, a.Equal(b))
		})
	}
}

// TestUnstructuredMapList_ToMapKey_BadElement covers the non-map element error
// path in toMapKey. A map-list whose element is not a map produces an error
// from getMap -> toMapKey, surfaced through Equal.
func TestUnstructuredMapList_ToMapKey_BadElement(t *testing.T) {
	s := mapListSchemaWithKeyCount(1)
	good := UnstructuredToVal([]interface{}{elem(1, "x")}, s)
	// The bad list has a scalar element where a map is expected.
	bad := &unstructuredMapList{
		unstructuredList: unstructuredList{elements: []interface{}{"not-a-map"}, itemsSchema: s.Items()},
		escapedKeyProps:  escapeKeyProps([]string{"k0"}),
	}
	// Comparing good (size 1) against bad (size 1) builds good's map and looks
	// up bad's serialized key, which won't match -> False (no panic/error).
	res := good.Equal(bad)
	assert.Equal(t, types.False, res)
}

// TestUnstructuredLists_NonListerOperand covers the MaybeNoSuchOverloadErr
// branches in the Equal/Add methods of all three list flavors.
func TestUnstructuredLists_NonListerOperand(t *testing.T) {
	atomic := UnstructuredToVal([]interface{}{int64(1)}, arraySchema(&spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{"integer"}},
	}))
	set := UnstructuredToVal([]interface{}{"a"}, setListSchema(&spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{"string"}},
	}))
	mapList := UnstructuredToVal([]interface{}{elem(1, "x")}, mapListSchemaWithKeyCount(1))

	cases := []struct {
		name string
		fn   func() ref.Val
	}{
		{"atomic Equal non-lister", func() ref.Val { return atomic.Equal(types.String("x")) }},
		{"atomic Add non-lister", func() ref.Val { return atomic.(traits.Adder).Add(types.String("x")) }},
		{"set Equal non-lister", func() ref.Val { return set.Equal(types.String("x")) }},
		{"set Add non-lister", func() ref.Val { return set.(traits.Adder).Add(types.String("x")) }},
		{"maplist Equal non-lister", func() ref.Val { return mapList.Equal(types.String("x")) }},
		{"maplist Add non-lister", func() ref.Val { return mapList.(traits.Adder).Add(types.String("x")) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.True(t, types.IsError(tc.fn()), "expected error for non-lister operand")
		})
	}
}

// TestUnstructuredSetList_Equal_MissingElement covers the "element not in set"
// branch of unstructuredSetList.Equal (same size, different members).
func TestUnstructuredSetList_Equal_MissingElement(t *testing.T) {
	s := setListSchema(&spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{"string"}},
	})
	a := UnstructuredToVal([]interface{}{"a", "b"}, s)
	b := UnstructuredToVal([]interface{}{"a", "c"}, s)
	assert.Equal(t, types.False, a.Equal(b))
}

// TestUnstructuredList_Value covers the Value() accessor.
func TestUnstructuredList_Value(t *testing.T) {
	s := arraySchema(&spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}})
	val := UnstructuredToVal([]interface{}{"a", "b"}, s)
	raw := val.Value()
	assert.Equal(t, []interface{}{"a", "b"}, raw)
}

// TestUnstructuredMap_ConvertToNative covers the map-conversion and error paths.
func TestUnstructuredMap_ConvertToNative(t *testing.T) {
	s := objectSchemaWithAdditionalProps(&spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{"string"}},
	})
	val := UnstructuredToVal(map[string]interface{}{"a": "1", "b": "2"}, s).(ref.Val)

	cases := []struct {
		name    string
		target  reflect.Type
		wantErr bool
		check   func(t *testing.T, out interface{})
	}{
		{
			name:   "assignable to interface map",
			target: reflect.TypeOf(map[string]interface{}{}),
			check: func(t *testing.T, out interface{}) {
				m, ok := out.(map[string]interface{})
				require.True(t, ok)
				assert.Equal(t, "1", m["a"])
			},
		},
		{
			name:   "convert to map[string]string",
			target: reflect.TypeOf(map[string]string{}),
			check: func(t *testing.T, out interface{}) {
				m, ok := out.(map[string]string)
				require.True(t, ok)
				assert.Equal(t, "1", m["a"])
			},
		},
		{
			name:    "non-map target errors",
			target:  reflect.TypeOf(0),
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := val.ConvertToNative(tc.target)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tc.check != nil {
				tc.check(t, out)
			}
		})
	}
}

// TestUnstructured_ConvertToType_Errors covers the unsupported-type branches of
// ConvertToType on both list and map.
func TestUnstructured_ConvertToType_Errors(t *testing.T) {
	listVal := UnstructuredToVal([]interface{}{"a"}, arraySchema(&spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{"string"}},
	}))
	mapVal := UnstructuredToVal(map[string]interface{}{"a": "1"}, objectSchemaWithAdditionalProps(&spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{"string"}},
	}))

	cases := []struct {
		name string
		got  ref.Val
	}{
		{"list to string type", listVal.ConvertToType(types.StringType)},
		{"map to string type", mapVal.ConvertToType(types.StringType)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.True(t, types.IsError(tc.got))
		})
	}
}

// TestUnstructuredMap_Find_NonStringKey covers the key-conversion branch in
// Find where the key is not already a types.String.
func TestUnstructuredMap_Find_NonStringKey(t *testing.T) {
	s := objectSchemaWithAdditionalProps(&spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{"string"}},
	})
	mapper := UnstructuredToVal(map[string]interface{}{"42": "answer"}, s).(traits.Mapper)

	// An int key is convertible to string "42" and should be found.
	v, found := mapper.Find(types.Int(42))
	assert.True(t, found)
	assert.Equal(t, types.String("answer"), v)
}

// TestUnstructuredMap_Find_UnconvertibleKey covers the branch where the key
// cannot be converted to a string at all.
func TestUnstructuredMap_Find_UnconvertibleKey(t *testing.T) {
	s := objectSchemaWithAdditionalProps(&spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{"string"}},
	})
	mapper := UnstructuredToVal(map[string]interface{}{"a": "1"}, s).(traits.Mapper)

	// A list value has no string conversion; Find should report an error val.
	v, found := mapper.Find(types.NewErr("boom"))
	assert.True(t, found)
	assert.True(t, types.IsError(v))
}

// TestUnstructuredMap_Equal_UnknownField covers the unknown-field fallback path
// in unstructuredMap.Equal where a key isn't in the property schema and the
// code defers to semantic deep-equality.
func TestUnstructuredMap_Equal_UnknownField(t *testing.T) {
	// Object schema declares only "known"; data carries an extra "extra" key.
	s := objectSchemaWithProps(map[string]spec.Schema{
		"known": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
	})
	data := map[string]interface{}{"known": "x", "extra": "y"}
	a := UnstructuredToVal(data, s)
	b := UnstructuredToVal(map[string]interface{}{"known": "x", "extra": "y"}, s)
	assert.Equal(t, types.True, a.Equal(b))

	c := UnstructuredToVal(map[string]interface{}{"known": "x", "extra": "DIFFERENT"}, s)
	assert.Equal(t, types.False, a.Equal(c))
}

// TestIsOneOfStringNumber covers the negative branches of isOneOfStringNumber.
func TestIsOneOfStringNumber(t *testing.T) {
	cases := []struct {
		name  string
		oneOf []spec.Schema
		want  bool
	}{
		{name: "empty", oneOf: nil, want: false},
		{
			name: "string and number",
			oneOf: []spec.Schema{
				{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				{SchemaProps: spec.SchemaProps{Type: []string{"number"}}},
			},
			want: true,
		},
		{
			name: "string and integer not matched",
			oneOf: []spec.Schema{
				{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				{SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
			},
			want: false,
		},
		{
			name: "three variants",
			oneOf: []spec.Schema{
				{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				{SchemaProps: spec.SchemaProps{Type: []string{"number"}}},
				{SchemaProps: spec.SchemaProps{Type: []string{"boolean"}}},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &openapi.Schema{Schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{OneOf: tc.oneOf},
			}}
			assert.Equal(t, tc.want, isOneOfStringNumber(s))
		})
	}
}
