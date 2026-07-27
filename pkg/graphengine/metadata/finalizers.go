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

// Package metadata holds small utilities that act on object metadata
// (finalizers, labels, annotations). Kept independent of any controller so
// reconcilers and tests can share the same helpers without import cycles.
package metadata

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// GraphFinalizer is the finalizer the Graph reconciler claims on each Graph
// so cleanup can run before the API server completes deletion. It is distinct
// from the RGD/instance finalizer (kro.run/finalizer) so the two controllers
// never contend over the same finalizer string.
const GraphFinalizer = expv1alpha1.KRODomainName + "/graph-finalizer"

// HasGraphFinalizer reports whether obj already carries the Graph finalizer.
func HasGraphFinalizer(obj metav1.Object) bool {
	return containsString(obj.GetFinalizers(), GraphFinalizer)
}

// SetGraphFinalizer appends the Graph finalizer if it isn't present.
func SetGraphFinalizer(obj metav1.Object) {
	if !HasGraphFinalizer(obj) {
		obj.SetFinalizers(append(obj.GetFinalizers(), GraphFinalizer))
	}
}

// RemoveGraphFinalizer drops the Graph finalizer if it's present.
func RemoveGraphFinalizer(obj metav1.Object) {
	obj.SetFinalizers(removeString(obj.GetFinalizers(), GraphFinalizer))
}

func containsString(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

func removeString(s []string, x string) []string {
	out := make([]string, 0, len(s))
	for _, v := range s {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}
