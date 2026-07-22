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
	"fmt"
	"math"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// DefaultMaxCollectionSize is the maximum number of instances a single
// forEach expansion may produce. Without a cap, a Graph like
// forEach: [{r: ${[1..100]}}, {t: ${[1..100]}}] would generate 10_000
// objects per reconcile — a runaway. kro caps this; we copy the default.
const DefaultMaxCollectionSize = 500

// identityKey returns the GVK + namespace + name string used to dedup
// rendered objects and to align observed-to-desired in collections.
// Caller must guarantee namespace and name are set.
func identityKey(obj *unstructured.Unstructured) string {
	gvk := obj.GroupVersionKind()
	return fmt.Sprintf("%s/%s/%s/%s/%s",
		gvk.Group, gvk.Version, gvk.Kind,
		obj.GetNamespace(), obj.GetName())
}

// validateUniqueIdentities returns an error if any two objects in objs
// share the same identityKey. Called after forEach expansion to catch
// the case where the user's identity-field expressions don't actually
// produce distinct names — otherwise SSA would reject the duplicates
// with a confusing field-manager error.
func validateUniqueIdentities(objs []*unstructured.Unstructured) error {
	seen := make(map[string]struct{}, len(objs))
	for _, obj := range objs {
		k := identityKey(obj)
		if _, dup := seen[k]; dup {
			return fmt.Errorf("duplicate identity in collection: %s", k)
		}
		seen[k] = struct{}{}
	}
	return nil
}

// orderedIntersection returns the observed items that also exist in
// desired, ordered to match desired's sequence. Used for collection
// nodes so readyWhen evaluation across instances is deterministic across
// reconciles even though the API LIST result isn't ordered.
func orderedIntersection(observed, desired []*unstructured.Unstructured) []*unstructured.Unstructured {
	if len(observed) == 0 || len(desired) == 0 {
		return observed
	}
	byKey := make(map[string]*unstructured.Unstructured, len(observed))
	for _, obj := range observed {
		byKey[identityKey(obj)] = obj
	}
	out := make([]*unstructured.Unstructured, 0, len(desired))
	for _, want := range desired {
		if got, ok := byKey[identityKey(want)]; ok {
			out = append(out, got)
		}
	}
	return out
}

// evaluatedDimension pairs a forEach iterator name with its evaluated
// source values. Internal type used by cartesianProduct.
type evaluatedDimension struct {
	name   string
	values []any
}

// cartesianProduct computes the cartesian product of multiple axes and
// caps the total combination count at maxSize. Each output row maps
// every iterator name to one value from its source list. Any zero-length
// source short-circuits the whole expansion to empty (the SQL outer-join
// semantics).
func cartesianProduct(dims []evaluatedDimension, maxSize int) ([]map[string]any, error) {
	if len(dims) == 0 {
		return []map[string]any{{}}, nil
	}
	total := 1
	for _, d := range dims {
		if len(d.values) == 0 {
			return nil, nil
		}
		// Catch overflow before the multiply, even when maxSize is
		// disabled (0). A malicious spec with five axes of 10^4 each
		// would wrap int and pass any post-multiply cap check.
		if d.values != nil && total > math.MaxInt/len(d.values) {
			return nil, fmt.Errorf("collection size overflows int (dimensions %d × %d)", total, len(d.values))
		}
		total *= len(d.values)
		if maxSize > 0 && total > maxSize {
			return nil, fmt.Errorf("collection size %d exceeds the maximum of %d", total, maxSize)
		}
	}
	out := make([]map[string]any, 0, total)
	idx := make([]int, len(dims))
	for {
		row := make(map[string]any, len(dims))
		for i, d := range dims {
			row[d.name] = d.values[idx[i]]
		}
		out = append(out, row)

		carry := true
		for i := len(idx) - 1; i >= 0 && carry; i-- {
			idx[i]++
			if idx[i] < len(dims[i].values) {
				carry = false
			} else {
				idx[i] = 0
			}
		}
		if carry {
			break
		}
	}
	return out, nil
}
