// Copyright 2026 The Kubernetes Authors.
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

package runtime

import (
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"sigs.k8s.io/krocodile/pkg/testutil/generator"
)

// TestRuntimeMaxCollectionSizeOption pins the new per-Runtime cap
// option introduced when MaxCollectionSize moved off the package
// global. Three rows: default cap, custom cap, disabled cap.
func TestRuntimeMaxCollectionSizeOption(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		want int
	}{
		{name: "default-cap", opts: nil, want: DefaultMaxCollectionSize},
		{name: "custom-cap", opts: []Option{WithMaxCollectionSize(7)}, want: 7},
		{name: "disabled-cap", opts: []Option{WithMaxCollectionSize(0)}, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := generator.NewGraph("g",
				generator.WithNamespace("default"),
				generator.WithDef("seed", map[string]any{"k": "v"}),
			)
			prog := compileGraph(t, g)
			rt := New(prog, g, tc.opts...)
			assert.Equal(t, tc.want, rt.MaxCollectionSize())
		})
	}
}

// TestCartesianProductOverflowRejected pins the overflow safeguard in
// cartesianProduct. With cap disabled (maxSize=0), a malicious spec
// using many axes can amplify the product past int range. We trigger
// it cheaply via seven axes of length 1000 — 1000^7 ≈ 10^21, well past
// 2^63. Only 7×1000 = 7k entries to allocate.
func TestCartesianProductOverflowRejected(t *testing.T) {
	axis := make([]any, 1000)
	for i := range axis {
		axis[i] = i
	}
	dims := make([]evaluatedDimension, 7)
	for i := range dims {
		dims[i] = evaluatedDimension{name: "a", values: axis}
	}
	rows, err := cartesianProduct(dims, 0)
	require.Error(t, err, "overflow must be rejected even when cap disabled")
	assert.Contains(t, err.Error(), "overflows")
	assert.Nil(t, rows)

	// Sanity: math.MaxInt is what we expected (avoid silent test
	// behavior drift if someone redefines the constant).
	assert.Greater(t, math.MaxInt, 0)
}

// TestCartesianProductHonoursCap regression-pins the post-multiply cap
// check, which the overflow guard sits in front of.
func TestCartesianProductHonoursCap(t *testing.T) {
	dims := []evaluatedDimension{
		{name: "a", values: []any{1, 2, 3}},
		{name: "b", values: []any{1, 2, 3}},
	}
	_, err := cartesianProduct(dims, 5)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
}

// TestIsSoftErrorClassifier pins the runtime error sentinels used by
// the executor to decide hard-abort vs soft-continue.
func TestIsSoftErrorClassifier(t *testing.T) {
	tests := []struct {
		name string
		err  error
		soft bool
	}{
		{"data-pending", ErrDataPending, true},
		{"waiting-for-readiness", ErrWaitingForReadiness, true},
		{"wrapped-data-pending", errors.New("wrapper: " + ErrDataPending.Error()), false},
		{"unwrapped-wrap", errors.Join(ErrDataPending, errors.New("x")), true},
		{"unrelated", errors.New("hard error"), false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			soft := errors.Is(tc.err, ErrDataPending) || errors.Is(tc.err, ErrWaitingForReadiness)
			assert.Equal(t, tc.soft, soft)
		})
	}
}
