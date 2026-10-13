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

package executor

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kubernetes-sigs/kro/pkg/cel/sentinels"
)

// resolveStableStamps replaces every time.now().stableString(...) marker in
// the rendered object with its latched RFC3339 value, using the pre-apply
// live object as the baseline. Called right after getLive so the stamp
// decision sees the same cluster state every other pre-apply guard sees.
// A nil current (object absent) is a first write.
func resolveStableStamps(obj, current *unstructured.Unstructured) {
	var live map[string]any
	if current != nil {
		live = current.Object
	}
	sentinels.ResolveStableStamps(obj.Object, live)
}
