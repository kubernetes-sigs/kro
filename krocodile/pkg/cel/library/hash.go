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

package library

import (
	"crypto/md5"
	"crypto/sha256"
	"hash/fnv"
	"math"
	"reflect"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// Hash returns a CEL library that provides hash functions for generating
// checksums and identifiers.
//
// Library functions:
//
// hash.fnv64a() computes the FNV-1a 64-bit hash of a string.
//
// The function takes one argument:
// - value: a string to hash
//
// Returns a bytes value containing the hash (8 bytes).
//
// FNV-1a is a fast, non-cryptographic hash function suitable for checksums,
// identifiers, and cache keys. It is the recommended hash function for
// all use cases in kro.
//
// Example usage:
//
//	base64.encode(hash.fnv64a('hello world'))
//	hash.fnv64a(schema.spec.data).base64()
//
// hash.sha256() computes the SHA-256 hash of a string.
//
// The function takes one argument:
// - value: a string to hash
//
// Returns a bytes value containing the hash (32 bytes).
//
// SHA-256 is provided for backwards compatibility only. Use hash.fnv()
// for new code unless you have a specific requirement for SHA-256.
//
// Example usage:
//
//	base64.encode(hash.sha256('hello world'))
//	hash.sha256(schema.spec.data).base64()
//
// hash.md5() computes the MD5 hash of a string.
//
// The function takes one argument:
// - value: a string to hash
//
// Returns a bytes value containing the hash (16 bytes).
//
// MD5 is provided for backwards compatibility only. Use hash.fnv()
// for new code unless you have a specific requirement for MD5.
//
// Example usage:
//
//	base64.encode(hash.md5('hello world'))
//	hash.md5(schema.spec.data).base64()
func Hash(options ...HashOption) cel.EnvOption {
	lib := &hashLibrary{version: math.MaxUint32}
	for _, o := range options {
		lib = o(lib)
	}
	return cel.Lib(lib)
}

type hashLibrary struct {
	version uint32
}

// HashOption is a functional option for configuring the hash library.
type HashOption func(*hashLibrary) *hashLibrary

// HashVersion configures the version of the hash library.
func HashVersion(version uint32) HashOption {
	return func(lib *hashLibrary) *hashLibrary {
		lib.version = version
		return lib
	}
}

func (l *hashLibrary) LibraryName() string {
	return "hash"
}

func (l *hashLibrary) CompileOptions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Function("hash.fnv64a",
			cel.Overload("hash.fnv64a_string",
				[]*cel.Type{cel.StringType},
				cel.BytesType,
				cel.UnaryBinding(fnv64aHash),
			),
		),
		cel.Function("hash.sha256",
			cel.Overload("hash.sha256_string",
				[]*cel.Type{cel.StringType},
				cel.BytesType,
				cel.UnaryBinding(sha256Hash),
			),
		),
		cel.Function("hash.md5",
			cel.Overload("hash.md5_string",
				[]*cel.Type{cel.StringType},
				cel.BytesType,
				cel.UnaryBinding(md5Hash),
			),
		),
	}
}

func (l *hashLibrary) ProgramOptions() []cel.ProgramOption {
	return nil
}

func fnv64aHash(value ref.Val) ref.Val {
	native, err := value.ConvertToNative(reflect.TypeOf(""))
	if err != nil {
		return types.NewErr("hash.fnv64a argument must be a string")
	}

	str, ok := native.(string)
	if !ok {
		return types.NewErr("hash.fnv64a argument must be a string")
	}

	h := fnv.New64a()
	h.Write([]byte(str))
	return types.Bytes(h.Sum(nil))
}

func sha256Hash(value ref.Val) ref.Val {
	native, err := value.ConvertToNative(reflect.TypeOf(""))
	if err != nil {
		return types.NewErr("hash.sha256 argument must be a string")
	}

	str, ok := native.(string)
	if !ok {
		return types.NewErr("hash.sha256 argument must be a string")
	}

	hash := sha256.Sum256([]byte(str))
	return types.Bytes(hash[:])
}

func md5Hash(value ref.Val) ref.Val {
	native, err := value.ConvertToNative(reflect.TypeOf(""))
	if err != nil {
		return types.NewErr("hash.md5 argument must be a string")
	}

	str, ok := native.(string)
	if !ok {
		return types.NewErr("hash.md5 argument must be a string")
	}

	hash := md5.Sum([]byte(str))
	return types.Bytes(hash[:])
}
