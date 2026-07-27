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

package sentinels

// Omit is a marker value that signals the resolver to remove the
// containing field or array element from the rendered object instead
// of writing a value.
type Omit struct{}

// IsOmit returns true if the value is an Omit sentinel.
func IsOmit(v interface{}) bool {
	_, ok := v.(Omit)
	return ok
}
