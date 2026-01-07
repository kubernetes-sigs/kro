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

package instance

import (
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/apis"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
)

const (
	Ready           = "Ready"
	InstanceManaged = "InstanceManaged"
	GraphResolved   = "GraphResolved"
	ResourcesReady  = "ResourcesReady"
)

var condSet = apis.NewReadyConditions(InstanceManaged, GraphResolved, ResourcesReady)

func NewConditionsMarkerFor(obj *unstructured.Unstructured) *ConditionsMarker {
	return &ConditionsMarker{
		cs: condSet.For(&unstructuredWrapper{obj}),
	}
}

type unstructuredWrapper struct {
	*unstructured.Unstructured
}

func (u *unstructuredWrapper) GetConditions() []v1alpha1.Condition {
	raw, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		return nil
	}

	b, _ := json.Marshal(raw)
	var out []v1alpha1.Condition
	_ = json.Unmarshal(b, &out)
	return out
}

func (u *unstructuredWrapper) SetConditions(conds []v1alpha1.Condition) {
	b, _ := json.Marshal(conds)
	var raw []interface{}
	_ = json.Unmarshal(b, &raw)
	_ = unstructured.SetNestedSlice(u.Object, raw, "status", "conditions")
}

type ConditionsMarker struct {
	cs apis.ConditionSet
}

// InstanceManaged signals the instance has proper finalizers and labels set.
func (m *ConditionsMarker) InstanceManaged() {
	m.cs.SetTrueWithReason(InstanceManaged, "Managed", "instance is properly managed with finalizers and labels")
}

// InstanceNotManaged signals there was an issue setting up the instance management.
func (m *ConditionsMarker) InstanceNotManaged(format string, a ...any) {
	m.cs.SetFalse(InstanceManaged, "ManagementFailed", fmt.Sprintf(format, a...))
}

// GraphResolved signals the runtime graph has been created and resources resolved.
func (m *ConditionsMarker) GraphResolved() {
	m.cs.SetTrueWithReason(GraphResolved, "Resolved", "runtime graph created and all resources resolved")
}

// GraphResolutionFailed signals there was an issue creating the runtime graph or resolving resources.
func (m *ConditionsMarker) GraphResolutionFailed(msg string, args ...any) {
	m.cs.SetFalse(GraphResolved, "ResolutionFailed", fmt.Sprintf(msg, args...))
}

// ResourcesReady signals all resources in the graph are created and ready.
func (m *ConditionsMarker) ResourcesReady() {
	m.cs.SetTrueWithReason(ResourcesReady, "AllResourcesReady", "all resources are created and ready")
}

// ResourcesNotReady signals there are resources in the graph that are not ready.
func (m *ConditionsMarker) ResourcesNotReady(msg string, args ...any) {
	m.cs.SetFalse(ResourcesReady, "NotReady", fmt.Sprintf(msg, args...))
}

// ResourcesUnderDeletion signals the controller is currently deleting resources.
func (m *ConditionsMarker) ResourcesUnderDeletion(msg string, args ...any) {
	m.cs.SetUnknownWithReason(ResourcesReady, "UnderDeletion", fmt.Sprintf(msg, args...))
}

func (c *Controller) updateStatus(rcx *ReconcileContext) error {
	rcx.updateInstanceState()
	status := rcx.initialStatus()

	inst := rcx.Runtime.GetInstance().DeepCopy()
	inst.Object["status"] = status

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := c.client.Dynamic().
			Resource(c.gvr).
			Namespace(inst.GetNamespace()).
			Get(rcx.Ctx, inst.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}
		cur.Object["status"] = status
		_, err = c.client.Dynamic().
			Resource(c.gvr).
			Namespace(inst.GetNamespace()).
			UpdateStatus(rcx.Ctx, cur, metav1.UpdateOptions{})
		return err
	})
}

func (rcx *ReconcileContext) initialStatus() map[string]interface{} {
	base := rcx.getExistingStatus()
	inst := rcx.Runtime.GetInstance()
	conds := condSet.For(&unstructuredWrapper{inst}).List()

	b, _ := json.Marshal(conds)
	var arr []interface{}
	_ = json.Unmarshal(b, &arr)

	base["conditions"] = arr
	if condSet.For(&unstructuredWrapper{inst}).IsRootReady() {
		base["state"] = InstanceStateActive
	} else {
		base["state"] = rcx.StateManager.State
	}
	return base
}

func (rcx *ReconcileContext) getExistingStatus() map[string]interface{} {
	out := map[string]interface{}{
		"conditions": []interface{}{},
	}
	raw, ok := rcx.Runtime.GetInstance().Object["status"].(map[string]interface{})
	if !ok {
		return out
	}
	for k, v := range raw {
		if k != "conditions" {
			out[k] = v
		}
	}
	return out
}

func (rcx *ReconcileContext) updateInstanceState() {
	switch rcx.StateManager.ReconcileErr.(type) {
	case *requeue.NoRequeue, *requeue.RequeueNeeded, *requeue.RequeueNeededAfter:
		return
	default:
		rcx.StateManager.Update()
	}
}
