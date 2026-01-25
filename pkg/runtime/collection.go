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

func validateUniqueIdentities(objs []*unstructured.Unstructured) error {
	seen := make(map[string]bool)
	for _, obj := range objs {
		key := identityKey(obj)
		if seen[key] {
			return fmt.Errorf("duplicate identity: %s", key)
		}
		seen[key] = true
	}
	return nil
}

func identityKey(obj *unstructured.Unstructured) string {
	// Caller must guarantee: cluster-scoped objects have empty namespace and
	// namespaced objects have a namespace set.
	gvk := obj.GroupVersionKind()
	return fmt.Sprintf("%s/%s/%s/%s/%s",
		gvk.Group, gvk.Version, gvk.Kind,
		obj.GetNamespace(), obj.GetName())
}

// orderedIntersection returns the observed items that also exist in desired,
// ordered to match desired's sequence.
//
// Why this exists:
//   - Kubernetes LIST results have no guaranteed order, so we cannot trust list order.
//   - Collection items are created/updated in a deterministic "desired" order
//     (the forEach expansion order). That order is the source of truth for collection
//     evaluation.
//   - readyWhen for collections evaluates "each" per item; aligning observed items
//     to desired indices keeps readiness checks deterministic across reconciles.
//   - Additional use cases will come with propagation control.
func orderedIntersection(
	observed, desired []*unstructured.Unstructured,
) []*unstructured.Unstructured {
	if len(observed) == 0 || len(desired) == 0 {
		return observed
	}

	observedByKey := make(map[string]*unstructured.Unstructured, len(observed))
	for _, obj := range observed {
		observedByKey[identityKey(obj)] = obj
	}

	result := make([]*unstructured.Unstructured, 0, len(desired))
	for _, obj := range desired {
		if observedObj, ok := observedByKey[identityKey(obj)]; ok {
			result = append(result, observedObj)
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
