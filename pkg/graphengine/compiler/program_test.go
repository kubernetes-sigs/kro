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

package compiler

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNodeKindString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind NodeKind
		want string
	}{
		{NodeKindTemplate, "template"},
		{NodeKindRef, "ref"},
		{NodeKindWatch, "watch"},
		{NodeKindDef, "def"},
		{NodeKind(99), "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.kind.String())
		})
	}
}

func TestNodeIsCollection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		node Node
		want bool
	}{
		{name: "template without forEach is single", node: Node{Kind: NodeKindTemplate}, want: false},
		{name: "template with forEach is a collection", node: Node{Kind: NodeKindTemplate, ForEach: []ForEachDimension{{Name: "r"}}}, want: true},
		{name: "watch is always a collection", node: Node{Kind: NodeKindWatch}, want: true},
		{name: "def without forEach is single", node: Node{Kind: NodeKindDef}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.node.IsCollection())
		})
	}
}
