// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package graphexec

import (
	"context"
	"encoding/json"
	"time"

	"github.com/aws/symphony/api/v1alpha1"
	"github.com/aws/symphony/internal/k8smetadata"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func (c *Controller) setManaged(ctx context.Context, uObj *unstructured.Unstructured, uid types.UID) (*unstructured.Unstructured, error) {
	c.log.V(1).Info("Setting managed", "resource", uObj.GetName(), "namespace", uObj.GetNamespace())

	dc := uObj.DeepCopy()
	_ = k8smetadata.SetInstanceFinalizerUnstructured(dc, uid)

	c.instanceLabeler.ApplyLabels(dc)

	if len(dc.GetFinalizers()) != len(uObj.GetFinalizers()) {
		b, _ := json.Marshal(dc)
		c.log.V(1).Info("Setting managed fffff", "resource", uObj.GetName(), "namespace", uObj.GetNamespace())

		patched, err := c.client.Resource(c.target).Namespace(uObj.GetNamespace()).Patch(ctx, uObj.GetName(), types.MergePatchType, b, metav1.PatchOptions{})
		if err != nil {
			return patched, err
		}
		return patched, nil
	}
	return uObj, nil
}

func (c *Controller) setUnmanaged(ctx context.Context, uObj *unstructured.Unstructured, uid types.UID) (*unstructured.Unstructured, error) {
	c.log.V(1).Info("Setting unmanaged", "resource", uObj.GetName(), "namespace", uObj.GetNamespace())

	dc := uObj.DeepCopy()
	_ = k8smetadata.RemoveInstanceFinalizerUnstructured(dc, uid)

	b, _ := json.Marshal(dc)
	patched, err := c.client.Resource(c.target).Namespace(uObj.GetNamespace()).Patch(ctx, uObj.GetName(), types.MergePatchType, b, metav1.PatchOptions{})
	if err != nil {
		return nil, err
	}
	return patched, nil
}

func (c *Controller) patchInstanceStatus(ctx context.Context, state string, conditions []*v1alpha1.Condition, extra map[string]interface{}) error {
	instanceUnstructuredCopy := c.runtime.Instance.DeepCopy()
	instanceUnstructuredCopy.SetResourceVersion("")
	s := map[string]interface{}{}
	if state != "" {
		s["state"] = state
	}
	if conditions != nil {
		s["conditions"] = conditions
	}
	for k, v := range extra {
		if k != "state" && k != "conditions" {
			s[k] = v
		}
	}

	instanceUnstructuredCopy.Object["status"] = s
	client := c.client.Resource(c.target)

	b, _ := json.MarshalIndent(instanceUnstructuredCopy, "", "  ")
	_, err := client.Namespace(instanceUnstructuredCopy.GetNamespace()).Patch(ctx, instanceUnstructuredCopy.GetName(), types.MergePatchType, b, metav1.PatchOptions{})
	if err != nil {
		return err
	}
	return nil
}

func (c *Controller) patchStatusError(ctx context.Context, err error) error {
	c.log.V(1).Info("Patching status error", "error", err)
	return c.patchInstanceStatus(ctx, "ERROR", []*v1alpha1.Condition{getReconcileErrorCondition(err)}, nil)
}

func getReconcileErrorCondition(err error) *v1alpha1.Condition {
	msg := err.Error()
	return &v1alpha1.Condition{
		Type:               v1alpha1.ConditionType("ResourceSynced"),
		Status:             corev1.ConditionFalse,
		LastTransitionTime: &metav1.Time{Time: time.Now()},
		Reason:             &msg,
		Message:            &msg,
	}
}
