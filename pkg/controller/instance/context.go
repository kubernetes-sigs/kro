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

	Runtime runtime.Interface
	Config  ReconcileConfig

	Mark         *ConditionsMarker
	StateManager *StateManager
}

// NewReconcileContext constructs a new sequential reconciliation context.
func NewReconcileContext(
	ctx context.Context,
	log logr.Logger,
	gvr schema.GroupVersionResource,
	c dynamic.Interface,
	r meta.RESTMapper,
	lbl metadata.Labeler,
	runtime runtime.Interface,
	cfg ReconcileConfig,
	instance *unstructured.Unstructured,
) *ReconcileContext {
	return &ReconcileContext{
		Ctx:          ctx,
		Log:          log,
		GVR:          gvr,
		Client:       c,
		RestMapper:   r,
		Labeler:      lbl,
		Runtime:      runtime,
		Config:       cfg,
		Mark:         NewConditionsMarkerFor(instance),
		StateManager: newStateManager(),
	}
}

func (rcx *ReconcileContext) delayedRequeue(err error) error {
	return requeue.NeededAfter(err, rcx.Config.DefaultRequeueDuration)
}

func (rcx *ReconcileContext) getResourceNamespace(resourceID string) string {
	res, _ := rcx.Runtime.GetResource(resourceID)
	if ns := res.GetNamespace(); ns != "" {
		return ns
	}
	inst := rcx.Runtime.GetInstance()
	if ns := inst.GetNamespace(); ns != "" {
		return ns
	}
	return metav1.NamespaceDefault
}

func (rcx *ReconcileContext) InstanceClient() dynamic.ResourceInterface {
	base := rcx.Client.Resource(rcx.GVR)
	if rcx.Runtime.GetInstance().GetNamespace() != "" {
		return base.Namespace(rcx.Runtime.GetInstance().GetNamespace())
	}
	return base
}
