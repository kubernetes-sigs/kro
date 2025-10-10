// Copyright 2025 The Kube Resource Orchestrator Authors
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

package delta

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Difference represents a single field-level difference between two objects.
// Path is the full path to the differing field (e.g. "spec.containers[0].image")
// Desired and Observed contain the actual values that differ at that path.
type Difference struct {
	// Path is the full path to the differing field (e.g. "spec.x.y.z"
	Path string `json:"path"`
	// Desired is the desired value at the path
	Desired interface{} `json:"desired"`
	// Observed is the observed value at the path
	Observed interface{} `json:"observed"`
}

// Compare takes desired and observed unstructured objects and returns a list of
// their differences. It performs a deep comparison while being aware of Kubernetes
// metadata specifics. The comparison:
//
// - Cleans metadata from both objects to avoid spurious differences
// - Walks object trees in parallel to find actual value differences
// - Builds path strings to precisely identify where differences occurs
// - Handles type mismatches, nil values, and empty vs nil collections
func Compare(desired, observed *unstructured.Unstructured) ([]Difference, error) {
	desiredCopy := desired.DeepCopy()
	observedCopy := observed.DeepCopy()

	cleanMetadata(desiredCopy)
	cleanMetadata(observedCopy)

	var differences []Difference
	walkCompare(desiredCopy.Object, observedCopy.Object, "", &differences)
	return differences, nil
}

// ignoredMetadataFields are Kubernetes metadata fields that should not trigger updates.
var ignoredMetadataFields = []string{
	"creationTimestamp",
	"deletionTimestamp",
	"generation",
	"resourceVersion",
	"selfLink",
	"uid",
	"managedFields",
	"ownerReferences",
	"finalizers",
}

// cleanMetadata removes Kubernetes metadata fields that should not trigger updates
// like resourceVersion, creationTimestamp, etc. Also handles empty maps in
// annotations and labels. This ensures we don't detect spurious changes based on
// Kubernetes-managed fields.
func cleanMetadata(obj *unstructured.Unstructured) {
	metadata, ok := obj.Object["metadata"].(map[string]interface{})
	if !ok {
		// Maybe we should panic here, but for now just return
		return
	}

	if annotations, exists := metadata["annotations"].(map[string]interface{}); exists {
		if len(annotations) == 0 {
			delete(metadata, "annotations")
		}
	}

	if labels, exists := metadata["labels"].(map[string]interface{}); exists {
		if len(labels) == 0 {
			delete(metadata, "labels")
		}
	}

	for _, field := range ignoredMetadataFields {
		delete(metadata, field)
	}
}

// walkCompare recursively compares desired and observed values, recording any
// differences found. It handles different types appropriately:
// - For maps: recursively compares all keys/values
// - For slices: checks length and recursively compares elements
// - For primitives: directly compares values
//
// Records a Difference if values don't match or are of different types.
func walkCompare(desired, observed interface{}, path string, differences *[]Difference) {
	switch d := desired.(type) {
	case map[string]interface{}:
		e, ok := observed.(map[string]interface{})
		if !ok {
			*differences = append(*differences, Difference{
				Path:     path,
				Observed: observed,
				Desired:  desired,
			})
			return
		}
		walkMap(d, e, path, differences)

	case []interface{}:
		e, ok := observed.([]interface{})
		if !ok {
			*differences = append(*differences, Difference{
				Path:     path,
				Observed: observed,
				Desired:  desired,
			})
			return
		}
		walkSlice(d, e, path, differences)

	default:
		if desired != observed {
			*differences = append(*differences, Difference{
				Path:     path,
				Observed: observed,
				Desired:  desired,
			})
		}
	}
}

// walkMap compares two maps recursively. For each key in desired:
//
// - If key missing in observed: records a difference
// - If key exists: recursively compares values
func walkMap(desired, observed map[string]interface{}, path string, differences *[]Difference) {
	for k, desiredVal := range desired {
		newPath := k
		if path != "" {
			newPath = fmt.Sprintf("%s.%s", path, k)
		}

		observedVal, exists := observed[k]
		if !exists && desiredVal != nil {
			*differences = append(*differences, Difference{
				Path:     newPath,
				Observed: nil,
				Desired:  desiredVal,
			})
			continue
		}

		walkCompare(desiredVal, observedVal, newPath, differences)
	}
}

// walkSlice compares two slices recursively:
// - If lengths differ: records entire slice as different
// - If lengths match: recursively compares elements
func walkSlice(desired, observed []interface{}, path string, differences *[]Difference) {
	if len(desired) != len(observed) {
		*differences = append(*differences, Difference{
			Path:     path,
			Observed: observed,
			Desired:  desired,
		})
		return
	}

	for i := range desired {
		newPath := fmt.Sprintf("%s[%d]", path, i)
		walkCompare(desired[i], observed[i], newPath, differences)
	}
}
