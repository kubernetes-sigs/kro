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

package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	internalv1alpha1 "github.com/kubernetes-sigs/kro/api/internal.kro.run/v1alpha1"
)

// RemoveKroLabelsAndFieldManagersToRetainResource removes KRO management labels
// and field managers from a resource in a single patch operation.
// Always removes all kro.run/* and internal.kro.run/* labels, plus applyset membership,
// and removes kro.run/applyset and kro.run/labeller field managers.
// Returns nil if resource not found.
func RemoveKroLabelsAndFieldManagersToRetainResource(
	ctx context.Context,
	client dynamic.Interface,
	gvr schema.GroupVersionResource,
	namespace string,
	name string,
) error {
	var rc dynamic.ResourceInterface
	if namespace != "" {
		rc = client.Resource(gvr).Namespace(namespace)
	} else {
		rc = client.Resource(gvr)
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := rc.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}

		var patchOps []map[string]interface{}

		// Add test operation for optimistic locking via resourceVersion
		patchOps = append(patchOps, map[string]interface{}{
			"op":    "test",
			"path":  "/metadata/resourceVersion",
			"value": current.GetResourceVersion(),
		})

		// Add label removal operations
		labels := current.GetLabels()
		if labels != nil {
			for key := range labels {
				if shouldRemoveLabel(key) {
					patchOps = append(patchOps, map[string]interface{}{
						"op":   "remove",
						"path": "/metadata/labels/" + escapeJSONPatchPath(key),
					})
				}
			}
		}

		// Add field manager removal operations
		if metadata, ok := current.Object["metadata"].(map[string]interface{}); ok {
			if managedFieldsRaw, ok := metadata["managedFields"]; ok {
				indices := findFieldManagerIndices(managedFieldsRaw, []string{
					"kro.run/applyset",
					"kro.run/labeller",
				})
				// Remove in descending order to prevent index shifting
				for i := len(indices) - 1; i >= 0; i-- {
					patchOps = append(patchOps, map[string]interface{}{
						"op":   "remove",
						"path": fmt.Sprintf("/metadata/managedFields/%d", indices[i]),
					})
				}
			}
		}

		// If only the test operation exists, nothing to patch
		if len(patchOps) == 1 {
			return nil
		}

		patchBytes, err := json.Marshal(patchOps)
		if err != nil {
			return err
		}

		_, err = rc.Patch(ctx, name, types.JSONPatchType, patchBytes, metav1.PatchOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	})
}

// shouldRemoveLabel returns true if the label should be removed during orphaning.
func shouldRemoveLabel(key string) bool {
	return strings.HasPrefix(key, LabelKROPrefix) ||
		strings.HasPrefix(key, internalv1alpha1.InternalKRODomainName+"/") ||
		key == "applyset.kubernetes.io/part-of" ||
		key == ManagedByLabelKey
}

// escapeJSONPatchPath escapes special characters for JSON Patch paths per RFC 6902.
// Replaces ~ with ~0 and / with ~1.
func escapeJSONPatchPath(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")
	return s
}

// findFieldManagerIndices returns the indices of managedFields entries that match
// the target field managers.
func findFieldManagerIndices(managedFieldsRaw interface{}, targetManagers []string) []int {
	managedFields, ok := managedFieldsRaw.([]interface{})
	if !ok {
		return nil
	}

	targetSet := make(map[string]bool)
	for _, m := range targetManagers {
		targetSet[m] = true
	}

	var indices []int
	for i, field := range managedFields {
		fieldMap, ok := field.(map[string]interface{})
		if !ok {
			continue
		}
		manager, ok := fieldMap["manager"].(string)
		if !ok {
			continue
		}
		if targetSet[manager] {
			indices = append(indices, i)
		}
	}
	return indices
}
