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
	"context"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
)

// DeletionContext contains only the dependencies available to the early
// deletion path. In particular, it has no runtime: deletion must remain
// independent of GraphRevision resolution and CEL evaluation.
type DeletionContext struct {
	Ctx context.Context
	Log logr.Logger

	GVR        schema.GroupVersionResource
	Namespaced bool
	Client     dynamic.Interface
	RestMapper meta.RESTMapper

	Instance *unstructured.Unstructured
	Config   ReconcileConfig

	WireStatus map[string]any

	Mark  *ConditionsMarker
	State v1alpha1.InstanceState
}

// NewDeletionContext constructs the runtime-free context used before graph
// resolution when an instance has a deletion timestamp.
func NewDeletionContext(
	ctx context.Context,
	log logr.Logger,
	gvr schema.GroupVersionResource,
	namespaced bool,
	client dynamic.Interface,
	restMapper meta.RESTMapper,
	config ReconcileConfig,
	instance *unstructured.Unstructured,
) *DeletionContext {
	return &DeletionContext{
		Ctx:        ctx,
		Log:        log,
		GVR:        gvr,
		Namespaced: namespaced,
		Client:     client,
		RestMapper: restMapper,
		Instance:   instance,
		Config:     config,
		WireStatus: captureWireStatus(instance),
		Mark:       NewConditionsMarkerFor(instance),
		State:      v1alpha1.InstanceStateInProgress,
	}
}

// rebindInstance replaces dcx.Instance with a fresh server response (e.g.
// after an SSA patch), re-capturing the wire status before the condition
// marker mutates the new object.
func (dcx *DeletionContext) rebindInstance(instance *unstructured.Unstructured) {
	dcx.Instance = instance
	dcx.WireStatus = captureWireStatus(instance)
	dcx.Mark = NewConditionsMarkerFor(instance)
}

// captureWireStatus deep-copies the instance's .status subtree.
func captureWireStatus(instance *unstructured.Unstructured) map[string]any {
	status, found, _ := unstructured.NestedMap(instance.Object, "status")
	if !found || status == nil {
		return nil
	}
	return runtime.DeepCopyJSON(status)
}

func (dcx *DeletionContext) delayedRequeue(err error) error {
	if dcx.Config.DefaultRequeueDuration == 0 {
		return requeue.None(err)
	}
	return requeue.NeededAfter(err, dcx.Config.DefaultRequeueDuration)
}

func (dcx *DeletionContext) InstanceClient() dynamic.ResourceInterface {
	base := dcx.Client.Resource(dcx.GVR)
	if dcx.Namespaced {
		return base.Namespace(dcx.Instance.GetNamespace())
	}
	return base
}

// instanceSSAPatch returns a minimal unstructured object for SSA patches
// targeting the instance. For cluster-scoped instances the namespace key is
// omitted so the API server does not receive an empty string.
func instanceSSAPatch(obj *unstructured.Unstructured) *unstructured.Unstructured {
	md := map[string]any{
		"name": obj.GetName(),
	}
	if ns := obj.GetNamespace(); ns != "" {
		md["namespace"] = ns
	}
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": obj.GetAPIVersion(),
			"kind":       obj.GetKind(),
			"metadata":   md,
		},
	}
}
