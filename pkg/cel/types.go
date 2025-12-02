/*
Copyright 2022 The Kubernetes Authors.

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

package cel

import (
	"fmt"
	"strconv"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"k8s.io/apimachinery/pkg/util/sets"
	apiservercel "k8s.io/apiserver/pkg/cel"
)

// FieldTypeMap constructs a map of the field and object types nested within a given type.
func FieldTypeMap(path string, t *apiservercel.DeclType) map[string]*apiservercel.DeclType {
	if t.IsObject() && t.TypeName() != "object" {
		path = t.TypeName()
	}
	types := make(map[string]*apiservercel.DeclType)
	buildDeclTypes(path, t, types)
	return types
}

func buildDeclTypes(path string, t *apiservercel.DeclType, types map[string]*apiservercel.DeclType) {
	// Ensure object types are properly named according to where they appear in the schema.
	if t.IsObject() {
		// Hack to ensure that names are uniquely qualified and work well with the type
		// resolution steps which require fully qualified type names for field resolution
		// to function properly.
		types[t.TypeName()] = t
		for name, field := range t.Fields {
			fieldPath := fmt.Sprintf("%s.%s", path, name)
			buildDeclTypes(fieldPath, field.Type, types)
		}
	}
	// Map element properties to type names if needed.
	if t.IsMap() {
		mapElemPath := fmt.Sprintf("%s.@elem", path)
		buildDeclTypes(mapElemPath, t.ElemType, types)
		types[path] = t
	}
	// List element properties.
	if t.IsList() {
		listIdxPath := fmt.Sprintf("%s.@idx", path)
		buildDeclTypes(listIdxPath, t.ElemType, types)
		types[path] = t
	}
}

func allTypesForDecl(declTypes []*apiservercel.DeclType) map[string]*apiservercel.DeclType {
	if declTypes == nil {
		return nil
	}
	allTypes := map[string]*apiservercel.DeclType{}
	for _, declType := range declTypes {
		for k, t := range FieldTypeMap(declType.TypeName(), declType) {
			allTypes[k] = t
		}
	}

	return allTypes
}

// NewDeclTypeProvider returns an Open API Schema-based type-system which is CEL compatible.
func NewDeclTypeProvider(rootTypes ...*apiservercel.DeclType) *DeclTypeProvider {
	// Note, if the schema indicates that it's actually based on another proto
	// then prefer the proto definition. For expressions in the proto, a new field
	// annotation will be needed to indicate the expected environment and type of
	// the expression.
	allTypes := allTypesForDecl(rootTypes)
	return &DeclTypeProvider{
		registeredTypes: allTypes,
	}
}

// DeclTypeProvider extends the CEL ref.TypeProvider interface and provides an Open API Schema-based
// type-system.
type DeclTypeProvider struct {
	registeredTypes             map[string]*apiservercel.DeclType
	typeProvider                types.Provider
	typeAdapter                 types.Adapter
	recognizeKeywordAsFieldName bool
}

func (rt *DeclTypeProvider) SetRecognizeKeywordAsFieldName(recognize bool) {
	rt.recognizeKeywordAsFieldName = recognize
}

func (rt *DeclTypeProvider) EnumValue(enumName string) ref.Val {
	return rt.typeProvider.EnumValue(enumName)
}

func (rt *DeclTypeProvider) FindIdent(identName string) (ref.Val, bool) {
	return rt.typeProvider.FindIdent(identName)
}

// EnvOptions returns a set of cel.EnvOption values which includes the declaration set
// as well as a custom ref.TypeProvider.
//
// If the DeclTypeProvider value is nil, an empty []cel.EnvOption set is returned.
func (rt *DeclTypeProvider) EnvOptions(tp types.Provider) ([]cel.EnvOption, error) {
	if rt == nil {
		return []cel.EnvOption{}, nil
	}
	rtWithTypes, err := rt.WithTypeProvider(tp)
	if err != nil {
		return nil, err
	}
	return []cel.EnvOption{
		cel.CustomTypeProvider(rtWithTypes),
		cel.CustomTypeAdapter(rtWithTypes),
	}, nil
}

// WithTypeProvider returns a new DeclTypeProvider that sets the given TypeProvider
// If the original DeclTypeProvider is nil, the returned DeclTypeProvider is still nil.
func (rt *DeclTypeProvider) WithTypeProvider(tp types.Provider) (*DeclTypeProvider, error) {
	if rt == nil {
		return nil, nil
	}
	var ta types.Adapter = types.DefaultTypeAdapter
	tpa, ok := tp.(types.Adapter)
	if ok {
		ta = tpa
	}
	rtWithTypes := &DeclTypeProvider{
		typeProvider:                tp,
		typeAdapter:                 ta,
		registeredTypes:             rt.registeredTypes,
		recognizeKeywordAsFieldName: rt.recognizeKeywordAsFieldName,
	}
	for name, declType := range rt.registeredTypes {
		tpType, found := tp.FindStructType(name)
		// cast celType to types.type

		expT := declType.CelType()
		if found && !expT.IsExactType(tpType) {
			return nil, fmt.Errorf(
				"type %s definition differs between CEL environment and type provider", name)
		}

	}
	return rtWithTypes, nil
}

// FindStructType attempts to resolve the typeName provided from the rule's rule-schema, or if not
// from the embedded ref.TypeProvider.
//
// FindStructType overrides the default type-finding behavior of the embedded TypeProvider.
//
// Note, when the type name is based on the Open API Schema, the name will reflect the object path
// where the type definition appears.
func (rt *DeclTypeProvider) FindStructType(typeName string) (*types.Type, bool) {
	if rt == nil {
		return nil, false
	}
	declType, found := rt.findDeclType(typeName)
	if found {
		expT := declType.CelType()
		return types.NewTypeTypeWithParam(expT), found
	}
	return rt.typeProvider.FindStructType(typeName)
}

// FindDeclType returns the CPT type description which can be mapped to a CEL type.
func (rt *DeclTypeProvider) FindDeclType(typeName string) (*apiservercel.DeclType, bool) {
	if rt == nil {
		return nil, false
	}
	return rt.findDeclType(typeName)
}

// FindStructFieldNames returns the field names associated with the type, if the type
// is found.
func (rt *DeclTypeProvider) FindStructFieldNames(typeName string) ([]string, bool) {
	return []string{}, false
}

// FindStructFieldType returns a field type given a type name and field name, if found.
//
// Note, the type name for an Open API Schema type is likely to be its qualified object path.
// If, in the future an object instance rather than a type name were provided, the field
// resolution might more accurately reflect the expected type model. However, in this case
// concessions were made to align with the existing CEL interfaces.
func (rt *DeclTypeProvider) FindStructFieldType(typeName, fieldName string) (*types.FieldType, bool) {
	st, found := rt.findDeclType(typeName)
	if !found {
		return rt.typeProvider.FindStructFieldType(typeName, fieldName)
	}

	f, found := st.Fields[fieldName]
	if rt.recognizeKeywordAsFieldName && !found && celReservedSymbols.Has(fieldName) {
		f, found = st.Fields["__"+fieldName+"__"]
	}

	if found {
		ft := f.Type
		expT := ft.CelType()
		return &types.FieldType{
			Type: expT,
		}, true
	}

	// This could be a dynamic map.
	if st.IsMap() {
		et := st.ElemType
		expT := et.CelType()
		return &types.FieldType{
			Type: expT,
		}, true
	}

	// This could be a dynamic object which has unknown fields that we want to make accessible dynamically.
	// This is more lenient than classic k8s but will offer kro the ability to expose the full CEL dynamic
	// type power.
	if value, found := st.Metadata[XKubernetesPreserveUnknownFields]; found {
		if preserveUnknown, err := strconv.ParseBool(value); err == nil && preserveUnknown {
			return &types.FieldType{
				Type: cel.DynType,
			}, true
		}
	}

	return nil, false
}

// celReservedSymbols is a list of RESERVED symbols defined in the CEL lexer.
// No identifiers are allowed to collide with these symbols.
// https://github.com/google/cel-spec/blob/master/doc/langdef.md#syntax
var celReservedSymbols = sets.NewString(
	"true", "false", "null", "in",
	"as", "break", "const", "continue", "else",
	"for", "function", "if", "import", "let",
	"loop", "package", "namespace", "return", // !! 'namespace' is used heavily in Kubernetes
	"var", "void", "while",
)

// NativeToValue is an implementation of the ref.TypeAdapter interface which supports conversion
// of rule values to CEL ref.Val instances.
func (rt *DeclTypeProvider) NativeToValue(val interface{}) ref.Val {
	return rt.typeAdapter.NativeToValue(val)
}

func (rt *DeclTypeProvider) NewValue(typeName string, fields map[string]ref.Val) ref.Val {
	// TODO: implement for OpenAPI types to enable CEL object instantiation, which is needed
	// for mutating admission.
	return rt.typeProvider.NewValue(typeName, fields)
}

// TypeNames returns the list of type names declared within the DeclTypeProvider object.
func (rt *DeclTypeProvider) TypeNames() []string {
	typeNames := make([]string, len(rt.registeredTypes))
	i := 0
	for name := range rt.registeredTypes {
		typeNames[i] = name
		i++
	}
	return typeNames
}

func (rt *DeclTypeProvider) findDeclType(typeName string) (*apiservercel.DeclType, bool) {
	declType, found := rt.registeredTypes[typeName]
	if found {
		return declType, true
	}
	declType = findScalar(typeName)
	return declType, declType != nil
}

func findScalar(typename string) *apiservercel.DeclType {
	switch typename {
	case apiservercel.BoolType.TypeName():
		return apiservercel.BoolType
	case apiservercel.BytesType.TypeName():
		return apiservercel.BytesType
	case apiservercel.DoubleType.TypeName():
		return apiservercel.DoubleType
	case apiservercel.DurationType.TypeName():
		return apiservercel.DurationType
	case apiservercel.IntType.TypeName():
		return apiservercel.IntType
	case apiservercel.NullType.TypeName():
		return apiservercel.NullType
	case apiservercel.StringType.TypeName():
		return apiservercel.StringType
	case apiservercel.TimestampType.TypeName():
		return apiservercel.TimestampType
	case apiservercel.UintType.TypeName():
		return apiservercel.UintType
	case apiservercel.ListType.TypeName():
		return apiservercel.ListType
	case apiservercel.MapType.TypeName():
		return apiservercel.MapType
	default:
		return nil
	}
}
