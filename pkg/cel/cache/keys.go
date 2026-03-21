// Copyright 2025 The Kubernetes Authors.
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

package cache

import (
	"encoding/hex"
	"encoding/json"
	"hash/fnv"
	"sort"
	"strings"

	"github.com/google/cel-go/cel"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// namedTypeCacheKey is the key for caching MaybeAssignTypeName results.
type namedTypeCacheKey struct {
	schema *spec.Schema
	name   string
}

// ProgramCacheKey keys compiled programs by (expression, *cel.Env).
type ProgramCacheKey struct {
	Expr string
	Env  *cel.Env
}

// ProgramCacheEntry holds a cached (program, AST) pair.
type ProgramCacheEntry struct {
	Program cel.Program
	Ast     *cel.Ast
}

// extendedEnvCacheKey keys extended environments by (parent, varName, schema).
type extendedEnvCacheKey struct {
	parentEnv *cel.Env
	varName   string
	schema    *spec.Schema
}

// iteratorEnvCacheKey keys iterator-extended environments by (parent, varsKey).
// varsKey is a canonical string encoding the iterator variable declarations.
type iteratorEnvCacheKey struct {
	parentEnv *cel.Env
	varsKey   string
}

// TypedEnvEntry holds a cached (env, provider) pair.
// Provider is stored as any to avoid importing pkg/cel (DeclTypeProvider).
type TypedEnvEntry struct {
	Env      *cel.Env
	Provider any
}

// MakeIteratorVarsKey builds a canonical string key from iterator variable
// names and their CEL type strings. Sorted by name for determinism.
func MakeIteratorVarsKey(vars map[string]*cel.Type) string {
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for i, name := range names {
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(vars[name].String())
	}
	return b.String()
}

type envSchemaKey struct {
	Type                  []string                 `json:"type,omitempty"`
	Format                string                   `json:"format,omitempty"`
	Required              []string                 `json:"required,omitempty"`
	XIntOrString          bool                     `json:"xIntOrString,omitempty"`
	PreserveUnknownFields bool                     `json:"preserveUnknownFields,omitempty"`
	Properties            map[string]*envSchemaKey `json:"properties,omitempty"`
	Items                 *envSchemaKey            `json:"items,omitempty"`
	AdditionalProperties  *envAdditionalPropsKey   `json:"additionalProperties,omitempty"`
}

type envAdditionalPropsKey struct {
	Allows bool          `json:"allows,omitempty"`
	Schema *envSchemaKey `json:"schema,omitempty"`
}

// MakeEnvCacheKey builds a canonical string key from a schema map.
// Schemas are keyed by their CEL-relevant structural shape rather than
// by pointer identity so equivalent schema sets reuse the same typed env.
func MakeEnvCacheKey(schemas map[string]*spec.Schema) string {
	names := make([]string, 0, len(schemas))
	for name := range schemas {
		names = append(names, name)
	}
	sort.Strings(names)

	h := fnv.New128a()
	normalized := make(map[string]*envSchemaKey, len(schemas))
	for _, name := range names {
		normalized[name] = makeEnvSchemaKey(schemas[name])
	}

	data, err := json.Marshal(normalized)
	if err == nil {
		_, _ = h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func makeEnvSchemaKey(schema *spec.Schema) *envSchemaKey {
	if schema == nil {
		return nil
	}

	key := &envSchemaKey{
		Type:                  append([]string(nil), schema.Type...),
		Format:                schema.Format,
		Required:              append([]string(nil), schema.Required...),
		XIntOrString:          extensionBool(schema.Extensions, "x-kubernetes-int-or-string"),
		PreserveUnknownFields: extensionBool(schema.Extensions, "x-kubernetes-preserve-unknown-fields"),
	}
	sort.Strings(key.Type)
	sort.Strings(key.Required)

	if len(schema.Properties) > 0 {
		key.Properties = make(map[string]*envSchemaKey, len(schema.Properties))
		for name, child := range schema.Properties {
			childCopy := child
			key.Properties[name] = makeEnvSchemaKey(&childCopy)
		}
	}

	if schema.Items != nil && schema.Items.Schema != nil {
		key.Items = makeEnvSchemaKey(schema.Items.Schema)
	}

	if schema.AdditionalProperties != nil {
		key.AdditionalProperties = &envAdditionalPropsKey{
			Allows: schema.AdditionalProperties.Allows,
			Schema: makeEnvSchemaKey(schema.AdditionalProperties.Schema),
		}
	}

	return key
}

func extensionBool(ext spec.Extensions, key string) bool {
	if ext == nil {
		return false
	}
	v, ok := ext[key]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}
