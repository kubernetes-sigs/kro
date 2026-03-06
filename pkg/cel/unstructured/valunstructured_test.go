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
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

func schema(typ string) *openapi.Schema {
	return &openapi.Schema{Schema: &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{typ},
		},
	}}
}

//nolint:unparam // currently only typ string is used in tests
func schemaWithFormat(typ, format string) *openapi.Schema {
	return &openapi.Schema{Schema: &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type:   []string{typ},
			Format: format,
		},
	}}
}

func nullableSchema(typ string) *openapi.Schema {
	return &openapi.Schema{Schema: &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type:     []string{typ},
			Nullable: true,
		},
	}}
}

func objectSchemaWithProps(props map[string]spec.Schema) *openapi.Schema {
	return &openapi.Schema{Schema: &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type:       []string{"object"},
			Properties: props,
		},
	}}
}

func objectSchemaWithAdditionalProps(valueSchema *spec.Schema) *openapi.Schema {
	return &openapi.Schema{Schema: &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			AdditionalProperties: &spec.SchemaOrBool{
				Allows: true,
				Schema: valueSchema,
			},
		},
	}}
}

func arraySchema(itemsSchema *spec.Schema) *openapi.Schema {
	return &openapi.Schema{Schema: &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"array"},
			Items: &spec.SchemaOrArray{
				Schema: itemsSchema,
			},
		},
	}}
}

func preserveUnknownFieldsSchema() *openapi.Schema {
	trueVal := true
	return &openapi.Schema{Schema: &spec.Schema{
		VendorExtensible: spec.VendorExtensible{
			Extensions: spec.Extensions{
				"x-kubernetes-preserve-unknown-fields": trueVal,
			},
		},
	}}
}

func intOrStringSchema() *openapi.Schema {
	trueVal := true
	return &openapi.Schema{Schema: &spec.Schema{
		VendorExtensible: spec.VendorExtensible{
			Extensions: spec.Extensions{
				"x-kubernetes-int-or-string": trueVal,
			},
		},
	}}
}

func mapListSchema(itemProps map[string]spec.Schema, mapKeys []string) *openapi.Schema {
	keys := make([]interface{}, len(mapKeys))
	for i, k := range mapKeys {
		keys[i] = k
	}
	return &openapi.Schema{Schema: &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"array"},
			Items: &spec.SchemaOrArray{
				Schema: &spec.Schema{
					SchemaProps: spec.SchemaProps{
						Type:       []string{"object"},
						Properties: itemProps,
					},
				},
			},
		},
		VendorExtensible: spec.VendorExtensible{
			Extensions: spec.Extensions{
				"x-kubernetes-list-type":     "map",
				"x-kubernetes-list-map-keys": keys,
			},
		},
	}}
}

func setListSchema(itemSchema *spec.Schema) *openapi.Schema {
	return &openapi.Schema{Schema: &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"array"},
			Items: &spec.SchemaOrArray{
				Schema: itemSchema,
			},
		},
		VendorExtensible: spec.VendorExtensible{
			Extensions: spec.Extensions{
				"x-kubernetes-list-type": "set",
			},
		},
	}}
}

func TestUnstructuredToVal_String(t *testing.T) {
	val := UnstructuredToVal("hello", schema("string"))
	assert.Equal(t, types.String("hello"), val)
}

func TestUnstructuredToVal_StringDateTime(t *testing.T) {
	val := UnstructuredToVal("2024-01-15T10:30:00Z", schemaWithFormat("string", "date-time"))
	ts, ok := val.(types.Timestamp)
	require.True(t, ok, "expected Timestamp, got %T", val)
	assert.Equal(t, 2024, ts.Time.Year())
	assert.Equal(t, time.Month(1), ts.Time.Month())
	assert.Equal(t, 15, ts.Time.Day())
}

func TestUnstructuredToVal_StringDate(t *testing.T) {
	val := UnstructuredToVal("2024-01-15", schemaWithFormat("string", "date"))
	ts, ok := val.(types.Timestamp)
	require.True(t, ok, "expected Timestamp, got %T", val)
	assert.Equal(t, 2024, ts.Time.Year())
}

func TestUnstructuredToVal_StringDuration(t *testing.T) {
	val := UnstructuredToVal("1h30m", schemaWithFormat("string", "duration"))
	dur, ok := val.(types.Duration)
	require.True(t, ok, "expected Duration, got %T", val)
	assert.Equal(t, 90*time.Minute, dur.Duration)
}

func TestUnstructuredToVal_StringBytes(t *testing.T) {
	// "aGVsbG8=" is base64("hello")
	val := UnstructuredToVal("aGVsbG8=", schemaWithFormat("string", "byte"))
	b, ok := val.(types.Bytes)
	require.True(t, ok, "expected Bytes, got %T", val)
	assert.Equal(t, []byte("hello"), []byte(b))
}

func TestUnstructuredToVal_Integer(t *testing.T) {
	tests := []struct {
		name  string
		input interface{}
	}{
		{"int", int(42)},
		{"int32", int32(42)},
		{"int64", int64(42)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val := UnstructuredToVal(tt.input, schema("integer"))
			assert.Equal(t, types.Int(42), val)
		})
	}
}

func TestUnstructuredToVal_Number(t *testing.T) {
	tests := []struct {
		name  string
		input interface{}
		want  types.Double
	}{
		{"float64", float64(3.14), types.Double(3.14)},
		{"float32", float32(3.14), types.Double(float32(3.14))},
		{"int_as_number", int(1), types.Double(1)},
		{"int64_as_number", int64(1), types.Double(1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val := UnstructuredToVal(tt.input, schema("number"))
			assert.Equal(t, tt.want, val)
		})
	}
}

func TestUnstructuredToVal_Boolean(t *testing.T) {
	assert.Equal(t, types.Bool(true), UnstructuredToVal(true, schema("boolean")))
	assert.Equal(t, types.Bool(false), UnstructuredToVal(false, schema("boolean")))
}

func TestUnstructuredToVal_NullNullable(t *testing.T) {
	val := UnstructuredToVal(nil, nullableSchema("string"))
	assert.Equal(t, types.NullValue, val)
}

func TestUnstructuredToVal_NullNotNullable(t *testing.T) {
	val := UnstructuredToVal(nil, schema("string"))
	assert.True(t, types.IsError(val), "expected error for null with nullable=false")
}

func TestUnstructuredToVal_ObjectWithProperties(t *testing.T) {
	s := objectSchemaWithProps(map[string]spec.Schema{
		"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
		"age":  {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
	})
	data := map[string]interface{}{
		"name": "alice",
		"age":  int64(30),
	}
	val := UnstructuredToVal(data, s)
	mapper, ok := val.(traits.Mapper)
	require.True(t, ok, "expected Mapper, got %T", val)

	nameVal, found := mapper.Find(types.String("name"))
	assert.True(t, found)
	assert.Equal(t, types.String("alice"), nameVal)

	ageVal, found := mapper.Find(types.String("age"))
	assert.True(t, found)
	assert.Equal(t, types.Int(30), ageVal)
}

func TestUnstructuredToVal_ObjectWithAdditionalProperties(t *testing.T) {
	s := objectSchemaWithAdditionalProps(&spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{"string"}},
	})
	data := map[string]interface{}{
		"key1": "val1",
		"key2": "val2",
	}
	val := UnstructuredToVal(data, s)
	mapper, ok := val.(traits.Mapper)
	require.True(t, ok, "expected Mapper, got %T", val)

	v1, found := mapper.Find(types.String("key1"))
	assert.True(t, found)
	assert.Equal(t, types.String("val1"), v1)
}

func TestUnstructuredToVal_ObjectUnknownFields(t *testing.T) {
	// Object with no properties and no additionalProperties — falls back to NativeToValue.
	s := schema("object")
	data := map[string]interface{}{
		"anything": "goes",
	}
	val := UnstructuredToVal(data, s)
	// NativeToValue wraps it as a native map. Just verify it's not an error.
	assert.False(t, types.IsError(val), "expected non-error for unknown fields object")
}

func TestUnstructuredToVal_ArrayWithItems(t *testing.T) {
	s := arraySchema(&spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{"string"}},
	})
	data := []interface{}{"a", "b", "c"}
	val := UnstructuredToVal(data, s)
	lister, ok := val.(traits.Lister)
	require.True(t, ok, "expected Lister, got %T", val)

	assert.Equal(t, types.Int(3), lister.Size())
	assert.Equal(t, types.String("a"), lister.Get(types.Int(0)))
	assert.Equal(t, types.String("b"), lister.Get(types.Int(1)))
	assert.Equal(t, types.String("c"), lister.Get(types.Int(2)))
}

func TestUnstructuredToVal_PreserveUnknownFields(t *testing.T) {
	s := preserveUnknownFieldsSchema()
	data := map[string]interface{}{
		"arbitrary": "data",
	}
	val := UnstructuredToVal(data, s)
	assert.False(t, types.IsError(val), "expected non-error for preserve-unknown-fields")
}

func TestUnstructuredToVal_IntOrString(t *testing.T) {
	s := intOrStringSchema()

	t.Run("string value", func(t *testing.T) {
		val := UnstructuredToVal("8080", s)
		assert.Equal(t, types.String("8080"), val)
	})
	t.Run("int value", func(t *testing.T) {
		val := UnstructuredToVal(int64(8080), s)
		assert.Equal(t, types.Int(8080), val)
	})
	t.Run("int32 value", func(t *testing.T) {
		val := UnstructuredToVal(int32(80), s)
		assert.Equal(t, types.Int(80), val)
	})
}

func TestUnstructuredToVal_TypeErrors(t *testing.T) {
	tests := []struct {
		name   string
		input  interface{}
		schema *openapi.Schema
	}{
		{"string expects string", int64(1), schema("string")},
		{"integer expects int", "not-int", schema("integer")},
		{"number expects numeric", "not-number", schema("number")},
		{"boolean expects bool", "not-bool", schema("boolean")},
		{"object expects map", "not-map", schema("object")},
		{"array expects slice", "not-slice", arraySchema(&spec.Schema{
			SchemaProps: spec.SchemaProps{Type: []string{"string"}},
		})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val := UnstructuredToVal(tt.input, tt.schema)
			assert.True(t, types.IsError(val), "expected error val for %s", tt.name)
		})
	}
}

func TestUnstructuredMap_Find_NullPropertyTreatedAsAbsent(t *testing.T) {
	// Simulates completionTime: null in observed data.
	// The null guard in Find should treat this as "not found" (data-pending),
	// not as an error from UnstructuredToVal(nil, ...).
	s := objectSchemaWithProps(map[string]spec.Schema{
		"completionTime": {SchemaProps: spec.SchemaProps{
			Type:   []string{"string"},
			Format: "date-time",
		}},
		"startTime": {SchemaProps: spec.SchemaProps{
			Type:   []string{"string"},
			Format: "date-time",
		}},
	})
	data := map[string]interface{}{
		"completionTime": nil,                    // null - should be treated as absent
		"startTime":      "2024-01-15T10:00:00Z", // present - should be found
	}
	val := UnstructuredToVal(data, s)
	mapper, ok := val.(traits.Mapper)
	require.True(t, ok, "expected Mapper, got %T", val)

	// Null property should return not-found (data pending), not an error.
	v, found := mapper.Find(types.String("completionTime"))
	assert.False(t, found, "null property should be treated as absent")
	assert.Nil(t, v, "null property should return nil value")

	// Non-null property should still be found.
	v, found = mapper.Find(types.String("startTime"))
	assert.True(t, found, "non-null property should be found")
	assert.False(t, types.IsError(v), "non-null property should not be error")

	// Get on null property should return "no such key" error.
	getResult := mapper.Get(types.String("completionTime"))
	assert.True(t, types.IsError(getResult), "Get on null property should return error")

	// Contains on null property should return false.
	containsResult := mapper.Contains(types.String("completionTime"))
	assert.Equal(t, types.False, containsResult)
}

func TestUnstructuredMap_Find_NullInAdditionalProperties(t *testing.T) {
	// For maps with additionalProperties (not object properties), null values
	// should NOT be treated as absent since isObject is false.
	s := objectSchemaWithAdditionalProps(&spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type:     []string{"string"},
			Nullable: true,
		},
	})
	data := map[string]interface{}{
		"key1": nil,
		"key2": "value",
	}
	val := UnstructuredToVal(data, s)
	mapper, ok := val.(traits.Mapper)
	require.True(t, ok, "expected Mapper, got %T", val)

	// In additionalProperties map, null with nullable=true is valid (not absent).
	v, found := mapper.Find(types.String("key1"))
	assert.True(t, found, "null in additionalProperties map should be found when nullable")
	assert.Equal(t, types.NullValue, v)
}

func TestUnstructuredList_Operations(t *testing.T) {
	s := arraySchema(&spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{"integer"}},
	})

	t.Run("Contains", func(t *testing.T) {
		val := UnstructuredToVal([]interface{}{int64(1), int64(2), int64(3)}, s)
		lister := val.(traits.Lister)
		assert.Equal(t, types.True, lister.Contains(types.Int(2)))
		assert.Equal(t, types.False, lister.Contains(types.Int(99)))
	})

	t.Run("Iterator", func(t *testing.T) {
		val := UnstructuredToVal([]interface{}{int64(10), int64(20)}, s)
		lister := val.(traits.Lister)
		it := lister.Iterator()

		assert.Equal(t, types.True, it.HasNext())
		assert.Equal(t, types.Int(10), it.Next())
		assert.Equal(t, types.True, it.HasNext())
		assert.Equal(t, types.Int(20), it.Next())
		assert.Equal(t, types.False, it.HasNext())
	})

	t.Run("Add", func(t *testing.T) {
		a := UnstructuredToVal([]interface{}{int64(1)}, s).(traits.Adder)
		b := UnstructuredToVal([]interface{}{int64(2)}, s)
		result := a.Add(b)
		lister := result.(traits.Lister)
		assert.Equal(t, types.Int(2), lister.Size())
	})
}

func TestUnstructuredMap_Iterator(t *testing.T) {
	s := objectSchemaWithAdditionalProps(&spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{"string"}},
	})
	data := map[string]interface{}{
		"a": "1",
		"b": "2",
	}
	val := UnstructuredToVal(data, s)
	mapper := val.(traits.Mapper)
	it := mapper.Iterator()

	keys := map[string]bool{}
	for it.HasNext() == types.True {
		k := it.Next()
		keys[k.Value().(string)] = true
	}
	assert.True(t, keys["a"])
	assert.True(t, keys["b"])
}

func TestUnstructuredMap_Equal(t *testing.T) {
	s := objectSchemaWithAdditionalProps(&spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{"string"}},
	})
	data := map[string]interface{}{
		"x": "1",
	}

	a := UnstructuredToVal(data, s)
	b := UnstructuredToVal(data, s)
	assert.Equal(t, types.True, a.Equal(b))

	c := UnstructuredToVal(map[string]interface{}{"x": "2"}, s)
	assert.Equal(t, types.False, a.Equal(c))
}

func TestUnstructuredToVal_ConvertToType(t *testing.T) {
	t.Run("map", func(t *testing.T) {
		s := objectSchemaWithAdditionalProps(&spec.Schema{
			SchemaProps: spec.SchemaProps{Type: []string{"string"}},
		})
		val := UnstructuredToVal(map[string]interface{}{"k": "v"}, s)
		assert.Equal(t, types.MapType, val.ConvertToType(types.TypeType))
		assert.Equal(t, val, val.ConvertToType(types.MapType))
	})

	t.Run("list", func(t *testing.T) {
		s := arraySchema(&spec.Schema{
			SchemaProps: spec.SchemaProps{Type: []string{"string"}},
		})
		val := UnstructuredToVal([]interface{}{"a"}, s)
		assert.Equal(t, types.ListType, val.ConvertToType(types.TypeType))
		assert.Equal(t, val, val.ConvertToType(types.ListType))
	})
}

// --- MapList tests (x-kubernetes-list-type=map) ---

func TestUnstructuredMapList_Equal(t *testing.T) {
	s := mapListSchema(map[string]spec.Schema{
		"name":  {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
		"value": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
	}, []string{"name"})

	t.Run("same elements different order", func(t *testing.T) {
		a := UnstructuredToVal([]interface{}{
			map[string]interface{}{"name": "x", "value": "1"},
			map[string]interface{}{"name": "y", "value": "2"},
		}, s)
		b := UnstructuredToVal([]interface{}{
			map[string]interface{}{"name": "y", "value": "2"},
			map[string]interface{}{"name": "x", "value": "1"},
		}, s)
		assert.Equal(t, types.True, a.Equal(b))
	})

	t.Run("different elements", func(t *testing.T) {
		a := UnstructuredToVal([]interface{}{
			map[string]interface{}{"name": "x", "value": "1"},
		}, s)
		b := UnstructuredToVal([]interface{}{
			map[string]interface{}{"name": "x", "value": "CHANGED"},
		}, s)
		assert.Equal(t, types.False, a.Equal(b))
	})

	t.Run("different sizes", func(t *testing.T) {
		a := UnstructuredToVal([]interface{}{
			map[string]interface{}{"name": "x", "value": "1"},
		}, s)
		b := UnstructuredToVal([]interface{}{
			map[string]interface{}{"name": "x", "value": "1"},
			map[string]interface{}{"name": "y", "value": "2"},
		}, s)
		assert.Equal(t, types.False, a.Equal(b))
	})
}

func TestUnstructuredMapList_Add(t *testing.T) {
	s := mapListSchema(map[string]spec.Schema{
		"name":  {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
		"value": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
	}, []string{"name"})

	t.Run("overlapping keys overwrite", func(t *testing.T) {
		a := UnstructuredToVal([]interface{}{
			map[string]interface{}{"name": "x", "value": "old"},
			map[string]interface{}{"name": "y", "value": "keep"},
		}, s)
		b := UnstructuredToVal([]interface{}{
			map[string]interface{}{"name": "x", "value": "new"},
		}, s)
		result := a.(traits.Adder).Add(b)
		lister := result.(traits.Lister)
		assert.Equal(t, types.Int(2), lister.Size())

		// x should be overwritten with "new"
		elem0 := lister.Get(types.Int(0))
		mapper0 := elem0.(traits.Mapper)
		v, found := mapper0.Find(types.String("value"))
		assert.True(t, found)
		assert.Equal(t, types.String("new"), v)

		// y should be preserved
		elem1 := lister.Get(types.Int(1))
		mapper1 := elem1.(traits.Mapper)
		v, found = mapper1.Find(types.String("value"))
		assert.True(t, found)
		assert.Equal(t, types.String("keep"), v)
	})

	t.Run("non-overlapping keys append", func(t *testing.T) {
		a := UnstructuredToVal([]interface{}{
			map[string]interface{}{"name": "x", "value": "1"},
		}, s)
		b := UnstructuredToVal([]interface{}{
			map[string]interface{}{"name": "y", "value": "2"},
		}, s)
		result := a.(traits.Adder).Add(b)
		lister := result.(traits.Lister)
		assert.Equal(t, types.Int(2), lister.Size())
	})
}

// --- SetList tests (x-kubernetes-list-type=set) ---

func TestUnstructuredSetList_Equal(t *testing.T) {
	s := setListSchema(&spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{"string"}},
	})

	t.Run("same elements different order", func(t *testing.T) {
		a := UnstructuredToVal([]interface{}{"a", "b", "c"}, s)
		b := UnstructuredToVal([]interface{}{"c", "a", "b"}, s)
		assert.Equal(t, types.True, a.Equal(b))
	})

	t.Run("different elements", func(t *testing.T) {
		a := UnstructuredToVal([]interface{}{"a", "b"}, s)
		b := UnstructuredToVal([]interface{}{"a", "z"}, s)
		assert.Equal(t, types.False, a.Equal(b))
	})

	t.Run("different sizes", func(t *testing.T) {
		a := UnstructuredToVal([]interface{}{"a"}, s)
		b := UnstructuredToVal([]interface{}{"a", "b"}, s)
		assert.Equal(t, types.False, a.Equal(b))
	})
}

func TestUnstructuredSetList_Add(t *testing.T) {
	s := setListSchema(&spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{"string"}},
	})

	t.Run("union no duplicates", func(t *testing.T) {
		a := UnstructuredToVal([]interface{}{"a", "b"}, s)
		b := UnstructuredToVal([]interface{}{"b", "c"}, s)
		result := a.(traits.Adder).Add(b)
		lister := result.(traits.Lister)
		assert.Equal(t, types.Int(3), lister.Size())
		assert.Equal(t, types.True, lister.Contains(types.String("a")))
		assert.Equal(t, types.True, lister.Contains(types.String("b")))
		assert.Equal(t, types.True, lister.Contains(types.String("c")))
	})

	t.Run("non-overlapping append", func(t *testing.T) {
		a := UnstructuredToVal([]interface{}{"a"}, s)
		b := UnstructuredToVal([]interface{}{"b", "c"}, s)
		result := a.(traits.Adder).Add(b)
		lister := result.(traits.Lister)
		assert.Equal(t, types.Int(3), lister.Size())
	})
}

// --- Object equality with properties ---

func TestUnstructuredMap_ObjectEquality(t *testing.T) {
	s := objectSchemaWithProps(map[string]spec.Schema{
		"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
		"age":  {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
	})

	t.Run("equal objects", func(t *testing.T) {
		a := UnstructuredToVal(map[string]interface{}{"name": "alice", "age": int64(30)}, s)
		b := UnstructuredToVal(map[string]interface{}{"name": "alice", "age": int64(30)}, s)
		assert.Equal(t, types.True, a.Equal(b))
	})

	t.Run("different property values", func(t *testing.T) {
		a := UnstructuredToVal(map[string]interface{}{"name": "alice", "age": int64(30)}, s)
		b := UnstructuredToVal(map[string]interface{}{"name": "bob", "age": int64(30)}, s)
		assert.Equal(t, types.False, a.Equal(b))
	})

	t.Run("different sizes", func(t *testing.T) {
		a := UnstructuredToVal(map[string]interface{}{"name": "alice", "age": int64(30)}, s)
		b := UnstructuredToVal(map[string]interface{}{"name": "alice"}, s)
		assert.Equal(t, types.False, a.Equal(b))
	})
}

// --- List equality (atomic) ---

func TestUnstructuredList_Equal(t *testing.T) {
	s := arraySchema(&spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{"integer"}},
	})

	t.Run("same elements same order", func(t *testing.T) {
		a := UnstructuredToVal([]interface{}{int64(1), int64(2), int64(3)}, s)
		b := UnstructuredToVal([]interface{}{int64(1), int64(2), int64(3)}, s)
		assert.Equal(t, types.True, a.Equal(b))
	})

	t.Run("different sizes", func(t *testing.T) {
		a := UnstructuredToVal([]interface{}{int64(1)}, s)
		b := UnstructuredToVal([]interface{}{int64(1), int64(2)}, s)
		assert.Equal(t, types.False, a.Equal(b))
	})

	t.Run("different elements", func(t *testing.T) {
		a := UnstructuredToVal([]interface{}{int64(1), int64(2)}, s)
		b := UnstructuredToVal([]interface{}{int64(1), int64(99)}, s)
		assert.Equal(t, types.False, a.Equal(b))
	})

	t.Run("order matters for atomic list", func(t *testing.T) {
		a := UnstructuredToVal([]interface{}{int64(1), int64(2)}, s)
		b := UnstructuredToVal([]interface{}{int64(2), int64(1)}, s)
		assert.Equal(t, types.False, a.Equal(b))
	})
}

// --- Map key-absent semantics ---

func TestUnstructuredMap_Find_AbsentKey(t *testing.T) {
	s := objectSchemaWithProps(map[string]spec.Schema{
		"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
		"age":  {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
	})
	data := map[string]interface{}{
		"name": "alice",
	}
	val := UnstructuredToVal(data, s)
	mapper := val.(traits.Mapper)

	t.Run("key not in data", func(t *testing.T) {
		v, found := mapper.Find(types.String("age"))
		assert.False(t, found)
		assert.Nil(t, v)
	})

	t.Run("key not in schema", func(t *testing.T) {
		v, found := mapper.Find(types.String("nonexistent"))
		assert.False(t, found)
		assert.Nil(t, v)
	})

	t.Run("Contains absent key", func(t *testing.T) {
		assert.Equal(t, types.False, mapper.Contains(types.String("age")))
	})
}

// --- List index error cases ---

func TestUnstructuredList_GetOutOfBounds(t *testing.T) {
	s := arraySchema(&spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{"string"}},
	})
	val := UnstructuredToVal([]interface{}{"a", "b"}, s)
	lister := val.(traits.Lister)

	t.Run("negative index", func(t *testing.T) {
		result := lister.Get(types.Int(-1))
		assert.True(t, types.IsError(result), "negative index should return error")
	})

	t.Run("beyond length", func(t *testing.T) {
		result := lister.Get(types.Int(99))
		assert.True(t, types.IsError(result), "index beyond length should return error")
	})
}

func TestUnstructuredList_GetNonIntIndex(t *testing.T) {
	s := arraySchema(&spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{"string"}},
	})
	val := UnstructuredToVal([]interface{}{"a", "b"}, s)
	lister := val.(traits.Lister)

	result := lister.Get(types.String("not-an-int"))
	assert.True(t, types.IsError(result), "non-int index should return error")
}

// --- Nested object access ---

func TestUnstructuredMap_NestedObject(t *testing.T) {
	s := objectSchemaWithProps(map[string]spec.Schema{
		"metadata": {
			SchemaProps: spec.SchemaProps{
				Type: []string{"object"},
				Properties: map[string]spec.Schema{
					"name":      {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"namespace": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				},
			},
		},
		"spec": {
			SchemaProps: spec.SchemaProps{
				Type: []string{"object"},
				Properties: map[string]spec.Schema{
					"replicas": {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
				},
			},
		},
	})
	data := map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":      "my-pod",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"replicas": int64(3),
		},
	}
	val := UnstructuredToVal(data, s)
	mapper := val.(traits.Mapper)

	// Access nested metadata.name via chained Find
	metadataVal, found := mapper.Find(types.String("metadata"))
	require.True(t, found)
	metadataMapper := metadataVal.(traits.Mapper)
	nameVal, found := metadataMapper.Find(types.String("name"))
	assert.True(t, found)
	assert.Equal(t, types.String("my-pod"), nameVal)

	// Access nested spec.replicas
	specVal, found := mapper.Find(types.String("spec"))
	require.True(t, found)
	specMapper := specVal.(traits.Mapper)
	replicasVal, found := specMapper.Find(types.String("replicas"))
	assert.True(t, found)
	assert.Equal(t, types.Int(3), replicasVal)
}

// --- ConvertToNative ---

func TestUnstructuredList_ConvertToNative(t *testing.T) {
	s := arraySchema(&spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{"string"}},
	})
	val := UnstructuredToVal([]interface{}{"hello", "world"}, s)

	t.Run("string list to []string", func(t *testing.T) {
		native, err := val.(ref.Val).ConvertToNative(reflect.TypeOf([]string{}))
		require.NoError(t, err)
		assert.Equal(t, []string{"hello", "world"}, native)
	})

	t.Run("unsupported target type", func(t *testing.T) {
		_, err := val.(ref.Val).ConvertToNative(reflect.TypeOf(0))
		assert.Error(t, err)
	})
}

// --- CEL expression evaluation tests ---

type typedValue struct {
	value  interface{}
	schema *openapi.Schema
}

// evalCEL compiles and evaluates a CEL expression against the provided variables.
// Each variable is wrapped with UnstructuredToVal using its schema.
func evalCEL(t *testing.T, expr string, vars map[string]typedValue) (ref.Val, error) {
	t.Helper()

	envOpts := make([]cel.EnvOption, 0, len(vars))
	for name := range vars {
		envOpts = append(envOpts, cel.Variable(name, cel.DynType))
	}
	env, err := cel.NewEnv(envOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("compile error: %w", issues.Err())
	}

	prg, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL program: %w", err)
	}

	activation := make(map[string]interface{}, len(vars))
	for name, tv := range vars {
		activation[name] = UnstructuredToVal(tv.value, tv.schema)
	}

	out, _, err := prg.Eval(activation)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func TestCELExpressionEvaluation(t *testing.T) {
	labelsSchema := objectSchemaWithAdditionalProps(&spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{"string"}},
	})
	tagsSchema := arraySchema(&spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{"string"}},
	})
	intSchema := schema("integer")
	stringSchema := schema("string")
	durationSchema := schemaWithFormat("string", "duration")
	dateTimeSchema := schemaWithFormat("string", "date-time")
	bytesSchema := schemaWithFormat("string", "byte")

	t.Run("map label access", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {
				value:  map[string]interface{}{"key1": "val1", "key2": "val2"},
				schema: labelsSchema,
			},
		}
		out, err := evalCEL(t, "c['key1'] == 'val1'", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("map 'in' operator", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {
				value:  map[string]interface{}{"key1": "val1", "key2": "val2"},
				schema: labelsSchema,
			},
		}
		out, err := evalCEL(t, "'key1' in c", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)

		out, err = evalCEL(t, "'key3' in c", vars)
		require.NoError(t, err)
		assert.Equal(t, types.False, out)
	})

	t.Run("map size", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {
				value:  map[string]interface{}{"key1": "val1", "key2": "val2"},
				schema: labelsSchema,
			},
		}
		out, err := evalCEL(t, "size(c) == 2", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("list index access", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {value: []interface{}{"a", "b", "c"}, schema: tagsSchema},
		}
		out, err := evalCEL(t, "c[1] == 'b'", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("list size", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {value: []interface{}{"a", "b", "c"}, schema: tagsSchema},
		}
		out, err := evalCEL(t, "size(c) == 3", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("list 'in' operator", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {value: []interface{}{"a", "b", "c"}, schema: tagsSchema},
		}
		out, err := evalCEL(t, "'b' in c", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)

		out, err = evalCEL(t, "'z' in c", vars)
		require.NoError(t, err)
		assert.Equal(t, types.False, out)
	})

	t.Run("list all macro", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {value: []interface{}{"a", "b", "c"}, schema: tagsSchema},
		}
		out, err := evalCEL(t, "c.all(t, t != '')", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("list exists macro", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {value: []interface{}{"a", "b", "c"}, schema: tagsSchema},
		}
		out, err := evalCEL(t, "c.exists(t, t == 'a')", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)

		out, err = evalCEL(t, "c.exists(t, t == 'z')", vars)
		require.NoError(t, err)
		assert.Equal(t, types.False, out)
	})

	t.Run("list add size", func(t *testing.T) {
		vars := map[string]typedValue{
			"c1": {value: []interface{}{"a", "b"}, schema: tagsSchema},
			"c2": {value: []interface{}{"c", "d", "e"}, schema: tagsSchema},
		}
		out, err := evalCEL(t, "size(c1 + c2) == 5", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("timestamp comparison", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {value: "2024-01-15T10:30:00Z", schema: dateTimeSchema},
		}
		out, err := evalCEL(t, "c == timestamp('2024-01-15T10:30:00Z')", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("duration comparison", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {value: "2s", schema: durationSchema},
		}
		out, err := evalCEL(t, "c > duration('1s')", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("bytes size", func(t *testing.T) {
		// "aGVsbG8=" is base64("hello") = 5 bytes
		vars := map[string]typedValue{
			"c": {value: "aGVsbG8=", schema: bytesSchema},
		}
		out, err := evalCEL(t, "size(c) == 5", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("type check string", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {value: "hello", schema: stringSchema},
		}
		out, err := evalCEL(t, "type(c) == string", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("type check int", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {value: int64(42), schema: intSchema},
		}
		out, err := evalCEL(t, "type(c) == int", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("type check list", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {value: []interface{}{"a"}, schema: tagsSchema},
		}
		out, err := evalCEL(t, "type(c) == list", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("type check map", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {value: map[string]interface{}{"k": "v"}, schema: labelsSchema},
		}
		out, err := evalCEL(t, "type(c) == map", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("string index on list errors", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {value: []interface{}{"a", "b"}, schema: tagsSchema},
		}
		_, err := evalCEL(t, "c['a']", vars)
		assert.Error(t, err, "string index on list should error")
	})

	t.Run("list out of bounds errors", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {value: []interface{}{"a", "b"}, schema: tagsSchema},
		}
		_, err := evalCEL(t, "c[99]", vars)
		assert.Error(t, err, "out of bounds index should error")
	})

	t.Run("set equality in CEL", func(t *testing.T) {
		setSchema := setListSchema(&spec.Schema{
			SchemaProps: spec.SchemaProps{Type: []string{"string"}},
		})
		vars := map[string]typedValue{
			"a": {value: []interface{}{"x", "y", "z"}, schema: setSchema},
			"b": {value: []interface{}{"z", "x", "y"}, schema: setSchema},
		}
		out, err := evalCEL(t, "a == b", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("set add in CEL", func(t *testing.T) {
		setSchema := setListSchema(&spec.Schema{
			SchemaProps: spec.SchemaProps{Type: []string{"string"}},
		})
		vars := map[string]typedValue{
			"a": {value: []interface{}{"x", "y"}, schema: setSchema},
			"b": {value: []interface{}{"y", "z"}, schema: setSchema},
		}
		out, err := evalCEL(t, "size(a + b) == 3", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("map list equality in CEL", func(t *testing.T) {
		mlSchema := mapListSchema(map[string]spec.Schema{
			"key": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
			"val": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
		}, []string{"key"})
		vars := map[string]typedValue{
			"a": {
				value: []interface{}{
					map[string]interface{}{"key": "k1", "val": "v1"},
					map[string]interface{}{"key": "k2", "val": "v2"},
				},
				schema: mlSchema,
			},
			"b": {
				value: []interface{}{
					map[string]interface{}{"key": "k2", "val": "v2"},
					map[string]interface{}{"key": "k1", "val": "v1"},
				},
				schema: mlSchema,
			},
		}
		out, err := evalCEL(t, "a == b", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("map list add in CEL", func(t *testing.T) {
		mlSchema := mapListSchema(map[string]spec.Schema{
			"key": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
			"val": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
		}, []string{"key"})
		vars := map[string]typedValue{
			"a": {
				value: []interface{}{
					map[string]interface{}{"key": "k1", "val": "old"},
				},
				schema: mlSchema,
			},
			"b": {
				value: []interface{}{
					map[string]interface{}{"key": "k1", "val": "new"},
					map[string]interface{}{"key": "k2", "val": "added"},
				},
				schema: mlSchema,
			},
		}
		out, err := evalCEL(t, "size(a + b) == 2", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("nested object field access", func(t *testing.T) {
		objSchema := objectSchemaWithProps(map[string]spec.Schema{
			"metadata": {
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					},
				},
			},
		})
		vars := map[string]typedValue{
			"c": {
				value: map[string]interface{}{
					"metadata": map[string]interface{}{"name": "test-pod"},
				},
				schema: objSchema,
			},
		}
		out, err := evalCEL(t, "c.metadata.name == 'test-pod'", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("boolean operations", func(t *testing.T) {
		boolSchema := schema("boolean")
		vars := map[string]typedValue{
			"c": {value: true, schema: boolSchema},
		}
		out, err := evalCEL(t, "c == true", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)

		out, err = evalCEL(t, "!c == false", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("arithmetic on numbers", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {value: int64(10), schema: intSchema},
		}
		out, err := evalCEL(t, "c + 5 == 15", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("string concatenation", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {value: "hello", schema: stringSchema},
		}
		out, err := evalCEL(t, "c + ' world' == 'hello world'", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("map list with multiple map keys", func(t *testing.T) {
		mlSchema := mapListSchema(map[string]spec.Schema{
			"ns":   {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
			"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
			"val":  {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
		}, []string{"ns", "name"})
		vars := map[string]typedValue{
			"a": {
				value: []interface{}{
					map[string]interface{}{"ns": "default", "name": "x", "val": "1"},
					map[string]interface{}{"ns": "kube-system", "name": "y", "val": "2"},
				},
				schema: mlSchema,
			},
			"b": {
				value: []interface{}{
					map[string]interface{}{"ns": "kube-system", "name": "y", "val": "2"},
					map[string]interface{}{"ns": "default", "name": "x", "val": "1"},
				},
				schema: mlSchema,
			},
		}
		out, err := evalCEL(t, "a == b", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("empty map size", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {value: map[string]interface{}{}, schema: labelsSchema},
		}
		out, err := evalCEL(t, "size(c) == 0", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("empty list size", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {value: []interface{}{}, schema: tagsSchema},
		}
		out, err := evalCEL(t, "size(c) == 0", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("list all on empty list", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {value: []interface{}{}, schema: tagsSchema},
		}
		out, err := evalCEL(t, "c.all(t, t != '')", vars)
		require.NoError(t, err)
		assert.Equal(t, types.True, out)
	})

	t.Run("list exists on empty list", func(t *testing.T) {
		vars := map[string]typedValue{
			"c": {value: []interface{}{}, schema: tagsSchema},
		}
		out, err := evalCEL(t, "c.exists(t, t == 'a')", vars)
		require.NoError(t, err)
		assert.Equal(t, types.False, out)
	})
}
