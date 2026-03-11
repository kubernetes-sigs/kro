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
	"sort"
	"strconv"
	"strings"
	"unsafe"

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

// TypedEnvEntry holds a cached (env, provider) pair.
// Provider is stored as any to avoid importing pkg/cel (DeclTypeProvider).
type TypedEnvEntry struct {
	Env      *cel.Env
	Provider any
}

// MakeEnvCacheKey builds a canonical string key from a schema map.
// Schemas are sorted by name for determinism, and each entry encodes
// the schema pointer address to distinguish different schema objects.
func MakeEnvCacheKey(schemas map[string]*spec.Schema) string {
	names := make([]string, 0, len(schemas))
	for name := range schemas {
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
		b.WriteString("0x")
		b.WriteString(strconv.FormatUint(uint64(uintptr(unsafe.Pointer(schemas[name]))), 16))
	}
	return b.String()
}
