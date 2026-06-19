/*
Copyright 2021 The Kubernetes Authors.

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

// Package unstructured provides schema-aware conversion of Kubernetes
// unstructured data to CEL ref.Val values. It is a port of the upstream
// k8s.io/apiserver/pkg/cel/common.UnstructuredToVal with adjustments for kro.
package unstructured

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apiserver/pkg/cel"
	"k8s.io/apiserver/pkg/cel/common"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
)

// UnstructuredToVal converts a Kubernetes unstructured data element to a CEL Val.
// The root schema of custom resource schemas is expected to contain type meta and object meta schemas.
// If Embedded resources do not contain type meta and object meta schemas, they will be added automatically.
// nolint:gocyclo // this should stay close to upstream [common.UnstructuredToVal]
func UnstructuredToVal(unstructured interface{}, schema common.Schema) ref.Val {
	if unstructured == nil {
		if schema.Nullable() {
			return types.NullValue
		}
		return types.NewErr("invalid data, got null for schema with nullable=false")
	}
	if schema.IsXIntOrString() {
		switch v := unstructured.(type) {
		case string:
			return types.String(v)
		case int:
			return types.Int(v)
		case int32:
			return types.Int(v)
		case int64:
			return types.Int(v)
		}
		return types.NewErr("invalid data, expected XIntOrString value to be either a string or integer")
	}
	if schema.Type() == "object" {
		m, ok := unstructured.(map[string]interface{})
		if !ok {
			return types.NewErr("invalid data, expected a map for the provided schema with type=object")
		}
		if schema.IsXEmbeddedResource() || schema.Properties() != nil {
			if schema.IsXEmbeddedResource() {
				schema = schema.WithTypeAndObjectMeta()
			}
			return &unstructuredMap{
				value:  m,
				schema: schema,
				propSchema: func(key string) (common.Schema, bool) {
					if schema, ok := schema.Properties()[key]; ok {
						return schema, true
					}
					return nil, false
				},
			}
		}
		if schema.AdditionalProperties() != nil && schema.AdditionalProperties().Schema() != nil {
			return &unstructuredMap{
				value:  m,
				schema: schema,
				propSchema: func(key string) (common.Schema, bool) {
					return schema.AdditionalProperties().Schema(), true
				},
			}
		}

		// Adjusted so KRO gives access to unknown fields best effort
		return types.DefaultTypeAdapter.NativeToValue(unstructured)
	}

	if schema.Type() == "array" {
		l, ok := unstructured.([]interface{})
		if !ok {
			return types.NewErr("invalid data, expected an array for the provided schema with type=array")
		}
		if schema.Items() == nil {
			return types.NewErr("invalid array type, expected Items with a non-empty Schema")
		}
		typedList := unstructuredList{elements: l, itemsSchema: schema.Items()}
		listType := schema.XListType()
		if listType != "" {
			switch listType {
			case "map":
				mapKeys := schema.XListMapKeys()
				return &unstructuredMapList{unstructuredList: typedList, escapedKeyProps: escapeKeyProps(mapKeys)}
			case "set":
				return &unstructuredSetList{unstructuredList: typedList}
			case "atomic":
				return &typedList
			default:
				return types.NewErr("invalid x-kubernetes-list-type, expected 'map', 'set' or 'atomic' but got %s", listType)
			}
		}
		return &typedList
	}

	if schema.Type() == "string" {
		str, ok := unstructured.(string)
		if !ok {
			return types.NewErr("invalid data, expected string, got %T", unstructured)
		}
		switch schema.Format() {
		case "duration":
			d, err := strfmt.ParseDuration(str)
			if err != nil {
				return types.NewErr("Invalid duration %s: %v", str, err)
			}
			return types.Duration{Duration: d}
		case "date":
			d, err := time.Parse(strfmt.RFC3339FullDate, str) // strfmt uses this format for OpenAPIv3 value validation
			if err != nil {
				return types.NewErr("Invalid date formatted string %s: %v", str, err)
			}
			return types.Timestamp{Time: d}
		case "date-time":
			d, err := strfmt.ParseDateTime(str)
			if err != nil {
				return types.NewErr("Invalid date-time formatted string %s: %v", str, err)
			}
			return types.Timestamp{Time: time.Time(d)}
		case "byte":
			base64 := strfmt.Base64{}
			err := base64.UnmarshalText([]byte(str))
			if err != nil {
				return types.NewErr("Invalid byte formatted string %s: %v", str, err)
			}
			return types.Bytes(base64)
		}

		return types.String(str)
	}
	if schema.Type() == "number" {
		switch v := unstructured.(type) {
		// float representations of whole numbers (e.g. 1.0, 0.0) can convert to int representations (e.g. 1, 0) in yaml
		// to json translation, and then get parsed as int64s
		case int:
			return types.Double(v)
		case int32:
			return types.Double(v)
		case int64:
			return types.Double(v)

		case float32:
			return types.Double(v)
		case float64:
			return types.Double(v)
		default:
			return types.NewErr("invalid data, expected float, got %T", unstructured)
		}
	}
	if schema.Type() == "integer" {
		switch v := unstructured.(type) {
		case int:
			return types.Int(v)
		case int32:
			return types.Int(v)
		case int64:
			return types.Int(v)
		default:
			return types.NewErr("invalid data, expected int, got %T", unstructured)
		}
	}
	if schema.Type() == "boolean" {
		b, ok := unstructured.(bool)
		if !ok {
			return types.NewErr("invalid data, expected bool, got %T", unstructured)
		}
		return types.Bool(b)
	}

	if schema.IsXPreserveUnknownFields() {
		// kro divergence: gives access to unknown fields best effort
		return types.DefaultTypeAdapter.NativeToValue(unstructured)
	}

	// kro divergence: resource.Quantity in OpenAPI v3 has no top-level type,
	// just oneOf: [{type: string}, {type: number}]. The x-kubernetes-int-or-string
	// extension is absent. Detect this pattern and let NativeToValue handle it.
	if schema.Type() == "" && isOneOfStringNumber(schema) {
		return types.DefaultTypeAdapter.NativeToValue(unstructured)
	}

	return types.NewErr("invalid type, expected object, array, number, integer, boolean or string, or no type with x-kubernetes-int-or-string or x-kubernetes-preserve-unknown-fields is true, got %s", schema.Type())
}

// isOneOfStringNumber returns true if the schema has exactly oneOf with
// "string" and "number" variants. This is how resource.Quantity appears
// in OpenAPI v3 schemas discovered from the cluster.
func isOneOfStringNumber(schema common.Schema) bool {
	oneOf := schema.OneOf()
	if len(oneOf) != 2 {
		return false
	}
	has := map[string]bool{oneOf[0].Type(): true, oneOf[1].Type(): true}
	return has["string"] && has["number"]
}

// unstructuredMapList represents an unstructured data instance of an OpenAPI array with x-kubernetes-list-type=map.
type unstructuredMapList struct {
	unstructuredList
	escapedKeyProps []string

	sync.Once // for for lazy load of mapOfList since it is only needed if Equals is called
	mapOfList map[interface{}]interface{}
}

func (t *unstructuredMapList) getMap() map[interface{}]interface{} {
	t.Do(func() {
		t.mapOfList = make(map[interface{}]interface{}, len(t.elements))
		for _, e := range t.elements {
			t.mapOfList[t.toMapKey(e)] = e
		}
	})
	return t.mapOfList
}

// toMapKey returns a valid golang map key for the given element of the map list.
// element must be a valid map list entry where all map key props are scalar types (which are comparable in go
// and valid for use in a golang map key).
func (t *unstructuredMapList) toMapKey(element interface{}) interface{} {
	eObj, ok := element.(map[string]interface{})
	if !ok {
		return types.NewErr("unexpected data format for element of array with x-kubernetes-list-type=map: %T", element)
	}
	// Arrays are comparable in go and may be used as map keys, but maps and slices are not.
	// So we can special case small numbers of key props as arrays and fall back to serialization
	// for larger numbers of key props
	if len(t.escapedKeyProps) == 1 {
		return eObj[t.escapedKeyProps[0]]
	}
	if len(t.escapedKeyProps) == 2 {
		return [2]interface{}{eObj[t.escapedKeyProps[0]], eObj[t.escapedKeyProps[1]]}
	}
	if len(t.escapedKeyProps) == 3 {
		return [3]interface{}{eObj[t.escapedKeyProps[0]], eObj[t.escapedKeyProps[1]], eObj[t.escapedKeyProps[2]]}
	}

	key := make([]interface{}, len(t.escapedKeyProps))
	for i, kf := range t.escapedKeyProps {
		key[i] = eObj[kf]
	}
	return fmt.Sprintf("%v", key)
}

// Equal on a map list ignores list element order.
func (t *unstructuredMapList) Equal(other ref.Val) ref.Val {
	oMapList, ok := other.(traits.Lister)
	if !ok {
		return types.MaybeNoSuchOverloadErr(other)
	}
	sz := types.Int(len(t.elements))
	if sz != oMapList.Size() {
		return types.False
	}
	tMap := t.getMap()
	for it := oMapList.Iterator(); it.HasNext() == types.True; {
		v := it.Next()
		k := t.toMapKey(v.Value())
		tVal, ok := tMap[k]
		if !ok {
			return types.False
		}
		eq := UnstructuredToVal(tVal, t.itemsSchema).Equal(v)
		if eq != types.True {
			return eq // either false or error
		}
	}
	return types.True
}

// Add for a map list `X + Y` performs a merge where the array positions of all keys in `X` are preserved but the values
// are overwritten by values in `Y` when the key sets of `X` and `Y` intersect. Elements in `Y` with
// non-intersecting keys are appended, retaining their partial order.
func (t *unstructuredMapList) Add(other ref.Val) ref.Val {
	oMapList, ok := other.(traits.Lister)
	if !ok {
		return types.MaybeNoSuchOverloadErr(other)
	}
	elements := make([]interface{}, len(t.elements))
	keyToIdx := map[interface{}]int{}
	for i, e := range t.elements {
		k := t.toMapKey(e)
		keyToIdx[k] = i
		elements[i] = e
	}
	for it := oMapList.Iterator(); it.HasNext() == types.True; {
		v := it.Next().Value()
		k := t.toMapKey(v)
		if overwritePosition, ok := keyToIdx[k]; ok {
			elements[overwritePosition] = v
		} else {
			elements = append(elements, v)
		}
	}
	return &unstructuredMapList{
		unstructuredList: unstructuredList{elements: elements, itemsSchema: t.itemsSchema},
		escapedKeyProps:  t.escapedKeyProps,
	}
}

// escapeKeyProps returns identifiers with Escape applied to each.
// Identifiers that cannot be escaped are left as-is. They are inaccessible to CEL programs but are
// are still needed internally to perform equality checks.
func escapeKeyProps(idents []string) []string {
	result := make([]string, len(idents))
	for i, prop := range idents {
		if escaped, ok := cel.Escape(prop); ok {
			result[i] = escaped
		} else {
			result[i] = prop
		}
	}
	return result
}

// unstructuredSetList represents an unstructured data instance of an OpenAPI array with x-kubernetes-list-type=set.
type unstructuredSetList struct {
	unstructuredList

	sync.Once // for lazy load of setOfList since it is only needed if Equals is called
	set       map[interface{}]struct{}
}

func (t *unstructuredSetList) getSet() map[interface{}]struct{} {
	// sets are only allowed to contain scalar elements, which are comparable in go, and can safely be used as
	// golang map keys
	t.Do(func() {
		t.set = make(map[interface{}]struct{}, len(t.elements))
		for _, e := range t.elements {
			t.set[e] = struct{}{}
		}
	})
	return t.set
}

// Equal on a map list ignores list element order.
func (t *unstructuredSetList) Equal(other ref.Val) ref.Val {
	oSetList, ok := other.(traits.Lister)
	if !ok {
		return types.MaybeNoSuchOverloadErr(other)
	}
	sz := types.Int(len(t.elements))
	if sz != oSetList.Size() {
		return types.False
	}
	tSet := t.getSet()
	for it := oSetList.Iterator(); it.HasNext() == types.True; {
		next := it.Next().Value()
		_, ok := tSet[next]
		if !ok {
			return types.False
		}
	}
	return types.True
}

// Add for a set list `X + Y` performs a union where the array positions of all elements in `X` are preserved and
// non-intersecting elements in `Y` are appended, retaining their partial order.
func (t *unstructuredSetList) Add(other ref.Val) ref.Val {
	oSetList, ok := other.(traits.Lister)
	if !ok {
		return types.MaybeNoSuchOverloadErr(other)
	}
	elements := t.elements
	set := t.getSet()
	for it := oSetList.Iterator(); it.HasNext() == types.True; {
		next := it.Next().Value()
		if _, ok := set[next]; !ok {
			set[next] = struct{}{}
			elements = append(elements, next)
		}
	}
	return &unstructuredSetList{
		unstructuredList: unstructuredList{elements: elements, itemsSchema: t.itemsSchema},
	}
}

// unstructuredList represents an unstructured data instance of an OpenAPI array with x-kubernetes-list-type=atomic (the default).
type unstructuredList struct {
	elements    []interface{}
	itemsSchema common.Schema
}

var _ = traits.Lister(&unstructuredList{})

func (t *unstructuredList) ConvertToNative(typeDesc reflect.Type) (interface{}, error) {
	// If the underlying list value is assignable to the reflected type return it.
	if reflect.TypeOf(t.elements).AssignableTo(typeDesc) {
		return t.elements, nil
	}
	// If the list wrapper is assignable to the desired type return it.
	if reflect.TypeOf(t).AssignableTo(typeDesc) {
		return t, nil
	}
	// Non-list conversion.
	if typeDesc.Kind() != reflect.Slice && typeDesc.Kind() != reflect.Array {
		return nil, fmt.Errorf("type conversion error from list to '%v'", typeDesc)
	}

	// List conversion.
	// Allow the element ConvertToNative() function to determine whether conversion is possible.
	otherElemType := typeDesc.Elem()
	elemCount := len(t.elements)
	var nativeList reflect.Value
	if typeDesc.Kind() == reflect.Array {
		nativeList = reflect.New(reflect.ArrayOf(elemCount, typeDesc)).Elem().Index(0)
	} else {
		nativeList = reflect.MakeSlice(typeDesc, elemCount, elemCount)
	}

	for i := 0; i < elemCount; i++ {
		elem := types.DefaultTypeAdapter.NativeToValue(t.elements[i])
		nativeElemVal, err := elem.ConvertToNative(otherElemType)
		if err != nil {
			return nil, err
		}
		nativeList.Index(i).Set(reflect.ValueOf(nativeElemVal))
	}
	return nativeList.Interface(), nil
}

func (t *unstructuredList) ConvertToType(typeValue ref.Type) ref.Val {
	switch typeValue {
	case types.ListType:
		return t
	case types.TypeType:
		return types.ListType
	}
	return types.NewErr("type conversion error from '%s' to '%s'", t.Type(), typeValue.TypeName())
}

func (t *unstructuredList) Equal(other ref.Val) ref.Val {
	oList, ok := other.(traits.Lister)
	if !ok {
		return types.MaybeNoSuchOverloadErr(other)
	}
	sz := types.Int(len(t.elements))
	if sz != oList.Size() {
		return types.False
	}
	for i := types.Int(0); i < sz; i++ {
		eq := t.Get(i).Equal(oList.Get(i))
		if eq != types.True {
			return eq // either false or error
		}
	}
	return types.True
}

func (t *unstructuredList) Type() ref.Type {
	return types.ListType
}

func (t *unstructuredList) Value() interface{} {
	return t.elements
}

func (t *unstructuredList) Add(other ref.Val) ref.Val {
	oList, ok := other.(traits.Lister)
	if !ok {
		return types.MaybeNoSuchOverloadErr(other)
	}
	elements := t.elements
	for it := oList.Iterator(); it.HasNext() == types.True; {
		next := it.Next().Value()
		elements = append(elements, next)
	}

	return &unstructuredList{elements: elements, itemsSchema: t.itemsSchema}
}

func (t *unstructuredList) Contains(val ref.Val) ref.Val {
	if types.IsUnknownOrError(val) {
		return val
	}
	var err ref.Val
	sz := len(t.elements)
	for i := 0; i < sz; i++ {
		elem := UnstructuredToVal(t.elements[i], t.itemsSchema)
		cmp := elem.Equal(val)
		b, ok := cmp.(types.Bool)
		if !ok && err == nil {
			err = types.MaybeNoSuchOverloadErr(cmp)
		}
		if b == types.True {
			return types.True
		}
	}
	if err != nil {
		return err
	}
	return types.False
}

func (t *unstructuredList) Get(idx ref.Val) ref.Val {
	iv, isInt := idx.(types.Int)
	if !isInt {
		return types.ValOrErr(idx, "unsupported index: %v", idx)
	}
	i := int(iv)
	if i < 0 || i >= len(t.elements) {
		return types.NewErr("index out of bounds: %v", idx)
	}
	return UnstructuredToVal(t.elements[i], t.itemsSchema)
}

func (t *unstructuredList) Iterator() traits.Iterator {
	items := make([]ref.Val, len(t.elements))
	for i, item := range t.elements {
		itemCopy := item
		items[i] = UnstructuredToVal(itemCopy, t.itemsSchema)
	}
	return &listIterator{unstructuredList: t, items: items}
}

type listIterator struct {
	*unstructuredList
	items []ref.Val
	idx   int
}

func (it *listIterator) HasNext() ref.Val {
	return types.Bool(it.idx < len(it.items))
}

func (it *listIterator) Next() ref.Val {
	item := it.items[it.idx]
	it.idx++
	return item
}

func (t *unstructuredList) Size() ref.Val {
	return types.Int(len(t.elements))
}

// unstructuredMap represented an unstructured data instance of an OpenAPI object.
type unstructuredMap struct {
	value  map[string]interface{}
	schema common.Schema
	// propSchema finds the schema to use for a particular map key.
	propSchema func(key string) (common.Schema, bool)
}

var _ = traits.Mapper(&unstructuredMap{})

func (t *unstructuredMap) ConvertToNative(typeDesc reflect.Type) (interface{}, error) {
	// If the map is already assignable to the desired type return it, e.g. interfaces and
	// maps with the same key value types.
	if reflect.TypeOf(t.value).AssignableTo(typeDesc) {
		return t.value, nil
	}
	if reflect.TypeOf(t).AssignableTo(typeDesc) {
		return t, nil
	}

	switch typeDesc.Kind() {
	// Map conversion.
	case reflect.Map:
		otherKey := typeDesc.Key()
		otherElem := typeDesc.Elem()
		nativeMap := reflect.MakeMapWithSize(typeDesc, len(t.value))
		it := t.Iterator()
		for it.HasNext() == types.True {
			key := it.Next()
			refKeyValue, err := key.ConvertToNative(otherKey)
			if err != nil {
				return nil, err
			}
			refElemValue, err := t.Get(key).ConvertToNative(otherElem)
			if err != nil {
				return nil, err
			}
			nativeMap.SetMapIndex(reflect.ValueOf(refKeyValue), reflect.ValueOf(refElemValue))
		}
		return nativeMap.Interface(), nil
	default:
		return nil, fmt.Errorf("type conversion error from map to '%v'", typeDesc)
	}
}

func (t *unstructuredMap) ConvertToType(typeValue ref.Type) ref.Val {
	switch typeValue {
	case types.MapType:
		return t
	case types.TypeType:
		return types.MapType
	}
	return types.NewErr("type conversion error from '%s' to '%s'", t.Type(), typeValue.TypeName())
}

func (t *unstructuredMap) Equal(other ref.Val) ref.Val {
	oMap, isMap := other.(traits.Mapper)
	if !isMap {
		return types.MaybeNoSuchOverloadErr(other)
	}
	if t.Size() != oMap.Size() {
		return types.False
	}

	for key, value := range t.value {
		if _, ok := t.propSchema(key); ok {
			v, found := t.Find(types.String(key))
			ov, oFound := oMap.Find(types.String(key))
			if found != oFound {
				return types.False
			}
			if !found {
				continue
			}
			vEq := v.Equal(ov)
			if vEq != types.True {
				return vEq // either false or error
			}
		} else {
			// Must be an object with properties.
			// Since we've encountered an unknown field, fallback to unstructured equality checking.
			ouMap, ok := other.(*unstructuredMap)
			if !ok {
				// The compiler ensures equality is against the same type of object, so this should be unreachable
				return types.MaybeNoSuchOverloadErr(other)
			}
			if oValue, ok := ouMap.value[key]; ok {
				if !equality.Semantic.DeepEqual(value, oValue) {
					return types.False
				}
			}
		}
	}
	return types.True
}

func (t *unstructuredMap) Type() ref.Type {
	return types.MapType
}

func (t *unstructuredMap) Value() interface{} {
	return t.value
}

func (t *unstructuredMap) Contains(key ref.Val) ref.Val {
	v, found := t.Find(key)
	if v != nil && types.IsUnknownOrError(v) {
		return v
	}

	return types.Bool(found)
}

func (t *unstructuredMap) Get(key ref.Val) ref.Val {
	v, found := t.Find(key)
	if found {
		return v
	}
	return types.ValOrErr(key, "no such key: %v", key)
}

func (t *unstructuredMap) Iterator() traits.Iterator {
	isObject := t.schema.Properties() != nil
	keys := make([]ref.Val, 0, len(t.value))
	for k := range t.value {
		mapKey := k
		if isObject {
			if escaped, ok := cel.Escape(k); ok {
				mapKey = escaped
			}
		}
		keys = append(keys, types.String(mapKey))
	}
	return &mapIterator{unstructuredMap: t, keys: keys}
}

type mapIterator struct {
	*unstructuredMap
	keys []ref.Val
	idx  int
}

func (it *mapIterator) HasNext() ref.Val {
	return types.Bool(it.idx < len(it.keys))
}

func (it *mapIterator) Next() ref.Val {
	key := it.keys[it.idx]
	it.idx++
	return key
}

func (t *unstructuredMap) Size() ref.Val {
	return types.Int(len(t.value))
}

func (t *unstructuredMap) Find(key ref.Val) (ref.Val, bool) {
	isObject := t.schema.Properties() != nil
	keyStr, ok := key.(types.String)
	if !ok {
		key = key.ConvertToType(types.StringType)
		if keyStr, ok = key.(types.String); !ok {
			return types.MaybeNoSuchOverloadErr(key), true
		}
	}
	k := keyStr.Value().(string)
	if isObject {
		k, ok = cel.Unescape(k)
		if !ok {
			return nil, false
		}
	}
	if v, ok := t.value[k]; ok {
		// Null values on object properties are treated as absent, matching
		// upstream k8s behavior.
		if isObject && v == nil {
			return nil, false
		}
		if propSchema, ok := t.propSchema(k); ok {
			return UnstructuredToVal(v, propSchema), true
		}
		return types.DefaultTypeAdapter.NativeToValue(v), true
	}

	return nil, false
}
