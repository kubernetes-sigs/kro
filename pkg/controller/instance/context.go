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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
)

type ReconcileContext struct {
	Ctx context.Context
	Log logr.Logger

	GVR        schema.GroupVersionResource
	Client     dynamic.Interface
	RestMapper meta.RESTMapper
	Labeler    metadata.Labeler

	Runtime  runtime.Interface
	Instance *unstructured.Unstructured
	Config   ReconcileConfig

	Mark         *ConditionsMarker
	StateManager *StateManager
}

// NewReconcileContext constructs a ReconcileContext for a single reconciliation cycle.
// It bundles all dependencies needed to reconcile an instance's resources:
//   - client/restMapper: for Kubernetes API operations
//   - labeler: for applying kro metadata labels to resources
//   - rt: the runtime containing resolved resource templates, and helpers to figure out
//     readiness, inclusion etc...
//   - instance: the instance CR being reconciled
//
// It also initializes internal state (Mark for conditions, StateManager for resource states).
func NewReconcileContext(
	ctx context.Context,
	log logr.Logger,
	gvr schema.GroupVersionResource,
	client dynamic.Interface,
	restMapper meta.RESTMapper,
	labeler metadata.Labeler,
	rt runtime.Interface,
	config ReconcileConfig,
	instance *unstructured.Unstructured,
) *ReconcileContext {
	return &ReconcileContext{
		Ctx:          ctx,
		Log:          log,
		GVR:          gvr,
		Client:       client,
		RestMapper:   restMapper,
		Labeler:      labeler,
		Runtime:      rt,
		Instance:     instance,
		Config:       config,
		Mark:         NewConditionsMarkerFor(instance),
		StateManager: newStateManager(),
	}
}

func (rcx *ReconcileContext) delayedRequeue(err error) error {
	return requeue.NeededAfter(err, rcx.Config.DefaultRequeueDuration)
}

// getResourceNamespace returns the namespace for a resource.
// Checks resource namespace first, falls back to instance namespace.
func (rcx *ReconcileContext) getResourceNamespace(desired *unstructured.Unstructured) string {
	if ns := desired.GetNamespace(); ns != "" {
		return ns
	}
	if ns := rcx.Instance.GetNamespace(); ns != "" {
		return ns
	}
	return metav1.NamespaceDefault
}

func (rcx *ReconcileContext) InstanceClient() dynamic.ResourceInterface {
	base := rcx.Client.Resource(rcx.GVR)
	if rcx.Instance.GetNamespace() != "" {
		return base.Namespace(rcx.Instance.GetNamespace())
	}
	return base
}
