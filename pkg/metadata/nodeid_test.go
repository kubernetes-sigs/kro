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

package metadata

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestNodeIDToken_PassesThroughPathsThatFit(t *testing.T) {
	t.Parallel()

	for path, want := range map[string]string{
		"res":                      "res",
		"myBucket":                 "myBucket",
		"subA/res":                 "subA.res",
		"outer/inner/deeplyNested": "outer.inner.deeplyNested",
		strings.Repeat("a", validation.LabelValueMaxLength): strings.Repeat("a", validation.LabelValueMaxLength),
	} {
		assert.False(t, NodeIDTokenIsHashed(path))
		assert.Equal(t, want, NodeIDToken(path))
	}
}

func TestNodeIDToken_HashesPathsThatOverflow(t *testing.T) {
	t.Parallel()

	id := strings.Repeat("a", validation.LabelValueMaxLength+1)
	got := NodeIDToken(id)

	assert.True(t, NodeIDTokenIsHashed(id))
	assert.NotEqual(t, id, got)
	assert.True(t, strings.HasPrefix(got, NodeIDHashPrefix))
	assert.LessOrEqual(t, len(got), validation.LabelValueMaxLength)
	assert.Empty(t, validation.IsValidLabelValue(got))
}

func TestNodeIDToken_CountsSeparatorsTowardsTheBudget(t *testing.T) {
	t.Parallel()

	seg := strings.Repeat("a", 21)
	path := strings.Join([]string{seg, seg, seg}, "/")

	assert.Len(t, path, validation.LabelValueMaxLength+2)
	assert.True(t, NodeIDTokenIsHashed(path))
}

func TestNodeIDToken_IsDeterministic(t *testing.T) {
	t.Parallel()

	id := "my" + strings.Repeat("Long", 40) + "NodeID"
	assert.Equal(t, NodeIDToken(id), NodeIDToken(id))
}

func TestNodeIDToken_DistinguishesSharedPrefixes(t *testing.T) {
	t.Parallel()

	prefix := strings.Repeat("a", validation.LabelValueMaxLength*2)
	assert.NotEqual(t, NodeIDToken(prefix+"one"), NodeIDToken(prefix+"two"))
}

func TestNodeIDToken_DistinguishesFramesFromDots(t *testing.T) {
	t.Parallel()

	assert.NotEqual(t, NodeIDToken(strings.Repeat("sub/", 20)+"res"),
		NodeIDToken(strings.Repeat("sub.", 20)+"res"))
}
