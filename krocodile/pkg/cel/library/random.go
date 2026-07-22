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
	"crypto/sha256"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

const (
	// alphanumericChars contains all possible characters for the random string
	alphanumericChars = "0123456789abcdefghijklmnopqrstuvwxyz"
)

// Random returns a CEL library that provides deterministic random generation
// functions.
//
// Library functions:
//
// random.seededString(length: int, seed: string) → string
//
// Generates a deterministic random alphanumeric string of the given length
// using the seed. Same length and seed always produce the same string.
//
//	random.seededString(10, schema.metadata.uid)
//
// random.seededInt(min: int, max: int, seed: string) → int
//
// Generates a deterministic random integer in [min, max) using the seed.
// Same min, max, and seed always produce the same integer.
//
//	random.seededInt(30000, 32768, schema.metadata.uid)
func Random() cel.EnvOption {
	return cel.Lib(&randomLibrary{})
}

type randomLibrary struct{}

func (l *randomLibrary) LibraryName() string {
	return "random"
}

func (l *randomLibrary) CompileOptions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Function("random.seededString",
			cel.Overload("random.seededString_int_string",
				[]*cel.Type{cel.IntType, cel.StringType},
				cel.StringType,
				cel.BinaryBinding(generateDeterministicString),
			),
		),
		cel.Function("random.seededInt",
			cel.Overload("random.seededInt_int_int_string",
				[]*cel.Type{cel.IntType, cel.IntType, cel.StringType},
				cel.IntType,
				cel.FunctionBinding(generateDeterministicInt),
			),
		),
	}
}

func (l *randomLibrary) ProgramOptions() []cel.ProgramOption {
	return nil
}

func generateDeterministicInt(args ...ref.Val) ref.Val {
	if len(args) != 3 {
		return types.NewErr("random.seededInt requires exactly 3 arguments")
	}
	minVal, maxVal, seed := args[0], args[1], args[2]

	if minVal.Type() != types.IntType {
		return types.NewErr("random.seededInt min must be an integer")
	}
	if maxVal.Type() != types.IntType {
		return types.NewErr("random.seededInt max must be an integer")
	}
	if seed.Type() != types.StringType {
		return types.NewErr("random.seededInt seed must be a string")
	}

	minInt := minVal.(types.Int).Value().(int64)
	maxInt := maxVal.(types.Int).Value().(int64)
	if minInt >= maxInt {
		return types.NewErr("random.seededInt min must be less than max")
	}

	seedStr := seed.(types.String).Value().(string)
	hash := sha256.Sum256([]byte(seedStr))

	// Read first 8 bytes as uint64 for better distribution
	v := uint64(hash[0])<<56 | uint64(hash[1])<<48 | uint64(hash[2])<<40 | uint64(hash[3])<<32 |
		uint64(hash[4])<<24 | uint64(hash[5])<<16 | uint64(hash[6])<<8 | uint64(hash[7])

	rangeSize := uint64(maxInt - minInt)
	result := minInt + int64(v%rangeSize)

	return types.Int(result)
}

func generateDeterministicString(length ref.Val, seed ref.Val) ref.Val {
	// Validate length is an integer
	if length.Type() != types.IntType {
		return types.NewErr("random.seededString length must be an integer")
	}

	// Validate length is positive
	if length.(types.Int) <= 0 {
		return types.NewErr("random.seededString length must be positive")
	}

	// Validate seed is a string
	if seed.Type() != types.StringType {
		return types.NewErr("random.seededString seed must be a string")
	}

	// Validate length
	lengthInt, ok := length.(types.Int)
	if !ok {
		return types.NewErr("random.seededString length must be an integer")
	}
	n := int(lengthInt.Value().(int64))
	if n <= 0 {
		return types.NewErr("random.seededString length must be positive")
	}

	// Validate seed
	seedStr, ok := seed.(types.String)
	if !ok {
		return types.NewErr("random.seededString seed must be a string")
	}

	// Generate hash from seed
	hash := sha256.Sum256([]byte(seedStr.Value().(string)))

	// Generate string from hash
	result := make([]byte, n)
	charsLen := len(alphanumericChars)
	for i := 0; i < n; i++ {
		// Use 4 bytes at a time from the hash
		start := (i * 4) % len(hash)
		end := start + 4
		if end > len(hash) {
			// If we run out of hash bytes, regenerate the hash with the current result
			newHash := sha256.Sum256(append(hash[:], result[:i]...))
			hash = newHash
			start = 0
		}
		// Convert 4 bytes to a uint32 and use it to select a character
		idx := uint32(hash[start])<<24 | uint32(hash[start+1])<<16 | uint32(hash[start+2])<<8 | uint32(hash[start+3])
		idx = idx % uint32(charsLen)
		result[i] = alphanumericChars[idx]
	}

	return types.String(string(result))
}
