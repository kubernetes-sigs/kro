// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package compat

// Ordered is a constraint that permits any ordered type: any type
// that supports the operators < <= >= >.
type OrderedType interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~uintptr |
		~float32 | ~float64 |
		~string
}

// Compare returns:
//   -1 if x < y
//    0 if x == y
//   +1 if x > y
func Compare[T OrderedType](x, y T) int {
	if x < y {
		return -1
	}
	if x > y {
		return +1
	}
	return 0
}

// Less reports whether x is less than y.
func Less[T OrderedType](x, y T) bool {
	return x < y
}

// Or returns a or b, with preference for a when not the zero value for T.
func Or[T comparable](a, b T) T {
	var zero T
	if a != zero {
		return a
	}
	return b
}