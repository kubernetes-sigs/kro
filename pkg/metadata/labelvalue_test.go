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
	"k8s.io/apimachinery/pkg/api/validate/content"
)

func TestEncodeLabelValue_PassesThroughValuesThatFit(t *testing.T) {
	t.Parallel()

	for _, s := range []string{
		"",
		"a",
		"myBucket",
		"my-instance.example.com",
		strings.Repeat("a", content.LabelValueMaxLength),
	} {
		assert.Equal(t, s, EncodeLabelValue(s), "value of length %d should be unchanged", len(s))
		assert.False(t, LabelValueOverflows(s))
	}
}

func TestEncodeLabelValue_EncodesValuesThatOverflow(t *testing.T) {
	t.Parallel()

	id := strings.Repeat("a", content.LabelValueMaxLength+1)
	got := EncodeLabelValue(id)

	assert.True(t, LabelValueOverflows(id))
	assert.NotEqual(t, id, got)
	assert.Equal(t, content.LabelValueMaxLength, len(got))
	assert.Empty(t, content.IsLabelValue(got))
	assert.Equal(t, strings.Repeat("a", labelValuePrefixWidth), got[:labelValuePrefixWidth])
}

func TestEncodeLabelValue_IsDeterministic(t *testing.T) {
	t.Parallel()

	id := "my" + strings.Repeat("Long", 40) + "NodeID"
	assert.Equal(t, EncodeLabelValue(id), EncodeLabelValue(id))
}

func TestEncodeLabelValue_DistinguishesSharedPrefixes(t *testing.T) {
	t.Parallel()
	prefix := strings.Repeat("a", content.LabelValueMaxLength*2)
	assert.NotEqual(t, EncodeLabelValue(prefix+"one"), EncodeLabelValue(prefix+"two"))
}

func TestEncodeLabelValue_ProducesValidLabelValues(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"lower camel node id":  "my" + strings.Repeat("Segment", 20),
		"dns subdomain name":   strings.Repeat("some-name.", 12) + "example.com",
		"cut lands on hyphen":  strings.Repeat("a", labelValuePrefixWidth-1) + "-" + strings.Repeat("b", 40),
		"cut lands on dot":     strings.Repeat("a", labelValuePrefixWidth-1) + "." + strings.Repeat("b", 40),
		"cut leaves separator": strings.Repeat("a", 10) + strings.Repeat(".", 60),
	}

	for name, id := range tests {
		t.Run(name, func(t *testing.T) {
			got := EncodeLabelValue(id)
			assert.Empty(t, content.IsLabelValue(got), "encoded %q is not a valid label value", got)
			assert.LessOrEqual(t, len(got), content.LabelValueMaxLength)
		})
	}
}

func TestEncodeLabelValue_FallsBackToHashWhenPrefixIsAllSeparators(t *testing.T) {
	t.Parallel()
	got := EncodeLabelValue(strings.Repeat(".", content.LabelValueMaxLength*2))
	assert.Len(t, got, labelValueHashWidth)
	assert.Empty(t, content.IsLabelValue(got))
}
