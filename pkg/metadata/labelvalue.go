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
	"encoding/hex"
	"hash/fnv"
	"strings"

	"k8s.io/apimachinery/pkg/api/validate/content"
)

const (
	// labelValueHashWidth is the hex width of a fnv32a sum.
	labelValueHashWidth = 8
	// labelValueSeparator joins the truncated prefix to the hash.
	labelValueSeparator = "-"
	// labelValuePrefixWidth is how much of the original value is left after truncation.
	labelValuePrefixWidth = content.LabelValueMaxLength - labelValueHashWidth - len(labelValueSeparator)
)

// LabelValueOverflows is used to check if the given label is over the max length.
// This is used for event generation to warn users that the label value is truncated.
func LabelValueOverflows(s string) bool {
	return len(s) > content.LabelValueMaxLength
}

// EncodeLabelValue returns `s` unchanged when it fits in a label value, and a
// truncated prefix joined to a fnv32a hash of the whole input when it does not.
//
// This is used conditionally on purpose. It will leave labels that already work
// alone, preventing a possible backwards compatibility issue, but will encode
// values that are too long, keeping that from breaking the flow.
func EncodeLabelValue(s string) string {
	// no-op if overflow is satisfied
	if !LabelValueOverflows(s) {
		return s
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	sum := hex.EncodeToString(h.Sum(nil))

	prefix := strings.TrimRight(s[:labelValuePrefixWidth], "._-")
	if prefix == "" {
		return sum
	}
	return prefix + labelValueSeparator + sum
}
