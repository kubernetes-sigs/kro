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

package sentinels

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsOmit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		val  any
		want bool
	}{
		{name: "omit value matches", val: Omit{}, want: true},
		{name: "pointer to omit does not match", val: &Omit{}, want: false},
		{name: "nil does not match", val: nil, want: false},
		{name: "string does not match", val: "hello", want: false},
		{name: "int does not match", val: 42, want: false},
		{name: "map does not match", val: map[string]any{}, want: false},
		{name: "empty struct (different type) does not match", val: struct{}{}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, IsOmit(tc.val))
		})
	}
}
