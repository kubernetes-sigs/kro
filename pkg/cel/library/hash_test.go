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
	"encoding/base64"
	"hash/fnv"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/ext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHashFNV64a(t *testing.T) {
	env, err := cel.NewEnv(Hash())
	require.NoError(t, err)

	testCases := []struct {
		name  string
		expr  string
		input string
	}{
		{
			name:  "hash hello world",
			expr:  `hash.fnv64a('hello world')`,
			input: "hello world",
		},
		{
			name:  "hash empty string",
			expr:  `hash.fnv64a('')`,
			input: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Compute expected value
			h := fnv.New64a()
			h.Write([]byte(tc.input))
			expected := h.Sum(nil)

			ast, issues := env.Compile(tc.expr)
			require.NoError(t, issues.Err())

			prg, err := env.Program(ast)
			require.NoError(t, err)

			out, _, err := prg.Eval(map[string]interface{}{})
			require.NoError(t, err)

			result := out.Value()
			assert.Equal(t, expected, result)
		})
	}
}

func TestHashSHA256(t *testing.T) {
	env, err := cel.NewEnv(Hash())
	require.NoError(t, err)

	testCases := []struct {
		name  string
		expr  string
		input string
	}{
		{
			name:  "hash hello world",
			expr:  `hash.sha256('hello world')`,
			input: "hello world",
		},
		{
			name:  "hash empty string",
			expr:  `hash.sha256('')`,
			input: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Compute expected value
			hash := sha256.Sum256([]byte(tc.input))

			ast, issues := env.Compile(tc.expr)
			require.NoError(t, issues.Err())

			prg, err := env.Program(ast)
			require.NoError(t, err)

			out, _, err := prg.Eval(map[string]interface{}{})
			require.NoError(t, err)

			result := out.Value()
			assert.Equal(t, hash[:], result)
		})
	}
}

func TestHashMD5(t *testing.T) {
	env, err := cel.NewEnv(Hash())
	require.NoError(t, err)

	testCases := []struct {
		name  string
		expr  string
		input string
	}{
		{
			name:  "hash hello world",
			expr:  `hash.md5('hello world')`,
			input: "hello world",
		},
		{
			name:  "hash empty string",
			expr:  `hash.md5('')`,
			input: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Compute expected value
			hash := md5.Sum([]byte(tc.input))

			ast, issues := env.Compile(tc.expr)
			require.NoError(t, issues.Err())

			prg, err := env.Program(ast)
			require.NoError(t, err)

			out, _, err := prg.Eval(map[string]interface{}{})
			require.NoError(t, err)

			result := out.Value()
			assert.Equal(t, hash[:], result)
		})
	}
}

func TestHashErrors(t *testing.T) {
	env, err := cel.NewEnv(Hash())
	require.NoError(t, err)

	testCases := []struct {
		name    string
		expr    string
		wantErr string
	}{
		{
			name:    "fnv64 non-string argument",
			expr:    `hash.fnv64a(123)`,
			wantErr: "found no matching overload",
		},
		{
			name:    "sha256 non-string argument",
			expr:    `hash.sha256(123)`,
			wantErr: "found no matching overload",
		},
		{
			name:    "md5 non-string argument",
			expr:    `hash.md5(123)`,
			wantErr: "found no matching overload",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ast, issues := env.Compile(tc.expr)
			if issues != nil && issues.Err() != nil {
				assert.Contains(t, issues.String(), tc.wantErr)
				return
			}

			prg, err := env.Program(ast)
			require.NoError(t, err)

			result, _, err := prg.Eval(map[string]interface{}{})
			if err == nil {
				t.Error("Expected error, got none")
			}
			if errVal, ok := result.(*types.Err); !ok || !assert.Contains(t, errVal.Error(), tc.wantErr) {
				t.Errorf("Expected error containing %q, got %v", tc.wantErr, result)
			}
		})
	}
}

func TestHashWithBase64Encoding(t *testing.T) {
	env, err := cel.NewEnv(Hash(), ext.Encoders())
	require.NoError(t, err)

	testCases := []struct {
		name     string
		expr     string
		input    string
		hashFunc func(string) []byte
	}{
		{
			name:  "sha256 with base64",
			expr:  `base64.encode(hash.sha256('hello world'))`,
			input: "hello world",
			hashFunc: func(s string) []byte {
				hash := sha256.Sum256([]byte(s))
				return hash[:]
			},
		},
		{
			name:  "md5 with base64",
			expr:  `base64.encode(hash.md5('hello world'))`,
			input: "hello world",
			hashFunc: func(s string) []byte {
				hash := md5.Sum([]byte(s))
				return hash[:]
			},
		},
		{
			name:  "fnv64 with base64",
			expr:  `base64.encode(hash.fnv64a('hello world'))`,
			input: "hello world",
			hashFunc: func(s string) []byte {
				h := fnv.New64a()
				h.Write([]byte(s))
				return h.Sum(nil)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Compute expected value
			hashBytes := tc.hashFunc(tc.input)
			expected := base64.StdEncoding.EncodeToString(hashBytes)

			ast, issues := env.Compile(tc.expr)
			require.NoError(t, issues.Err())

			prg, err := env.Program(ast)
			require.NoError(t, err)

			out, _, err := prg.Eval(map[string]interface{}{})
			require.NoError(t, err)

			result := out.Value()
			assert.Equal(t, expected, result)
		})
	}
}

func TestHashVersion(t *testing.T) {
	lib := &hashLibrary{}
	HashVersion(1)(lib)
	assert.Equal(t, uint32(1), lib.version)
}
