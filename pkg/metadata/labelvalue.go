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
	"crypto/sha256"
	"encoding/hex"

	"k8s.io/apimachinery/pkg/api/validate/content"
)

// HashedValuePrefix marks a label value kro had to hash. The leading letter
// also keeps the value a valid label value.
const HashedValuePrefix = "h-"

// LabelValueToken returns v when it fits in a label value, and a stable,
// collision-resistant hash of v when it does not. Kubernetes caps a label
// value at 63 characters while names and API groups are DNS subdomains (253),
// so any value kro copies into a label has to be bounded here or the apiserver
// rejects the apply.
//
// This is designed to be called as is so every kro label encodes the same way.
// NodeIDToken is an exception because it measures the '.'-joined path
// but hashes the '/'-form so distinct frames cannot collide.
func LabelValueToken(v string) string {
	if !LabelValueNeedsHashing(v) {
		return v
	}
	return hashedLabelValue(v)
}

// LabelValueNeedsHashing reports whether LabelValueToken will hash v rather
// than return it unchanged.
func LabelValueNeedsHashing(v string) bool {
	return len(v) > content.LabelValueMaxLength
}

// hashedLabelValue renders v as "h-<40 hex>", 42 characters total.
func hashedLabelValue(v string) string {
	sum := sha256.Sum256([]byte(v))
	return HashedValuePrefix + hex.EncodeToString(sum[:20])
}
