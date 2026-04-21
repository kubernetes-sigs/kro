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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	internalv1alpha1 "github.com/kubernetes-sigs/kro/api/internal.kro.run/v1alpha1"
)

// RemoveKroLabelsToRetainResource removes KRO management labels from a resource.
// Always removes all kro.run/* and internal.kro.run/* labels, plus applyset membership.
// Returns nil if resource not found.
func RemoveKroLabelsToRetainResource(
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

		labels := current.GetLabels()
		if labels == nil {
			return nil
		}

		labelsToRemove := make(map[string]interface{})
		for key := range labels {
			if strings.HasPrefix(key, LabelKROPrefix) ||
				strings.HasPrefix(key, internalv1alpha1.InternalKRODomainName+"/") ||
				key == "applyset.kubernetes.io/part-of" ||
				key == ManagedByLabelKey {
				labelsToRemove[key] = nil
			}
		}

		if len(labelsToRemove) == 0 {
			return nil
		}

		patch := map[string]interface{}{
			"metadata": map[string]interface{}{
				"labels":          labelsToRemove,
				"resourceVersion": current.GetResourceVersion(),
			},
		}

		patchBytes, err := json.Marshal(patch)
		if err != nil {
			return err
		}

		_, err = rc.Patch(ctx, name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	})
}
