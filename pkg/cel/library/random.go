// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

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

// Random returns a CEL library that provides functions to generate random text
//
// Library functions:
//
// random.seededString() returns a CEL function that generates deterministic random
// strings based on a seed.
//
// The function takes two arguments:
// - length: an integer specifying the length of the random string to generate
// - seed: a string used as the seed for the random string generation
//
// Example usage:
//
//	random.seededString(10, schema.metadata.uid)
//
// This will generate a random string of length 10 using the seed schema.metadata.uid.
// The same length and seed will always produce the same random string.
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
	}
}

func (l *randomLibrary) ProgramOptions() []cel.ProgramOption {
	return nil
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
