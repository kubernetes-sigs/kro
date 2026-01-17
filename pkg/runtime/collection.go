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

package runtime

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

func validateUniqueIdentities(objs []*unstructured.Unstructured, namespaced bool) error {
	seen := make(map[string]bool)
	for _, obj := range objs {
		if obj == nil {
			continue
		}
		key := identityKey(obj, namespaced)
		if seen[key] {
			return fmt.Errorf("duplicate identity: %s", key)
		}
		seen[key] = true
	}
	return nil
}

func identityKey(obj *unstructured.Unstructured, namespaced bool) string {
	if obj == nil {
		return ""
	}
	gvk := obj.GroupVersionKind()
	if namespaced {
		return fmt.Sprintf("%s/%s/%s/%s/%s",
			gvk.Group, gvk.Version, gvk.Kind,
			obj.GetNamespace(), obj.GetName())
	}
	// Cluster-scoped: namespace is irrelevant
	return fmt.Sprintf("%s/%s/%s/%s",
		gvk.Group, gvk.Version, gvk.Kind, obj.GetName())
}

func orderedCollectionObserved(
	observed, desired []*unstructured.Unstructured, namespaced bool,
) []*unstructured.Unstructured {
	if len(observed) == 0 || len(desired) == 0 {
		return observed
	}

	indexByKey := make(map[string]int, len(desired))
	for i, obj := range desired {
		if obj == nil {
			continue
		}
		indexByKey[identityKey(obj, namespaced)] = i
	}

	ordered := make([]*unstructured.Unstructured, len(desired))
	for _, obj := range observed {
		if obj == nil {
			continue
		}
		idx, ok := indexByKey[identityKey(obj, namespaced)]
		if !ok {
			continue // orphan
		}
		if ordered[idx] == nil {
			ordered[idx] = obj
		}
	}

	result := make([]*unstructured.Unstructured, 0, len(ordered))
	for _, obj := range ordered {
		if obj != nil {
			result = append(result, obj)
		}
	}
	return result
}

// evaluatedDimension pairs a forEach dimension name with its evaluated values.
type evaluatedDimension struct {
	name   string
	values []any
}

// cartesianProduct computes the cartesian product of multiple dimensions.
// Each dimension's Values are iterated over, producing all combinations.
// Values can be any type - scalars, lists, maps - they are not flattened.
func cartesianProduct(dimensions []evaluatedDimension) []map[string]any {
	if len(dimensions) == 0 {
		return nil
	}

	total := 1
	for _, dim := range dimensions {
		if len(dim.values) == 0 {
			return nil
		}
		total *= len(dim.values)
	}

	result := make([]map[string]any, 0, total)
	indices := make([]int, len(dimensions))

	for {
		combination := make(map[string]any, len(dimensions))
		for i, dim := range dimensions {
			combination[dim.name] = dim.values[indices[i]]
		}
		result = append(result, combination)

		carry := true
		for i := len(indices) - 1; i >= 0 && carry; i-- {
			indices[i]++
			if indices[i] < len(dimensions[i].values) {
				carry = false
			} else {
				indices[i] = 0
			}
		}
		if carry {
			break
		}
	}

	return result
}

func setCollectionIndexLabel(obj *unstructured.Unstructured, index int) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[metadata.CollectionIndexLabel] = fmt.Sprintf("%d", index)
	obj.SetLabels(labels)
}
