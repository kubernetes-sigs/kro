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

package instance

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"

	kroclient "github.com/kubernetes-sigs/kro/pkg/client"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
)

// ReconcileConfig holds configuration parameters for the reconciliation process.
// It allows the customization of various aspects of the controller's behavior.
type ReconcileConfig struct {
	// DefaultRequeueDuration is the default duration to wait before requeuing a
	// a reconciliation if no specific requeue time is set.
	DefaultRequeueDuration time.Duration
	// DeletionGraceTimeDuration is the duration to wait after initializing a resource
	// deletion before considering it failed
	// Not implemented.
	DeletionGraceTimeDuration time.Duration
	// DeletionPolicy is the deletion policy to use when deleting resources in the graph
	// TODO(a-hilaly): need to define think the different deletion policies we need to
	// support.
	DeletionPolicy string
}

// Controller manages the reconciliation of a single instance of a ResourceGraphDefinition,
// / it is responsible for reconciling the instance and its sub-resources.
//
// The controller is responsible for the following:
// - Reconciling the instance
// - Reconciling the sub-resources of the instance
// - Updating the status of the instance
// - Managing finalizers, owner references and labels
// - Handling errors and retries
// - Performing cleanup operations (garbage collection)
//
// For each instance of a ResourceGraphDefinition, the controller creates a new instance of
// the InstanceGraphReconciler to manage the reconciliation of the instance and its
// sub-resources.
//
// It is important to state that when the controller is reconciling an instance, it
// creates and uses a new instance of the ResourceGraphDefinitionRuntime to uniquely manage
// the state of the instance and its sub-resources. This ensure that at each
// reconciliation loop, the controller is working with a fresh state of the instance
// and its sub-resources.
// Controller owns reconciliation for instances of a ResourceGraphDefinition.
type Controller struct {
	log    logr.Logger
	client kroclient.SetInterface
	gvr    schema.GroupVersionResource
	rgd    *graph.Graph

	labeler         metadata.Labeler
	reconcileConfig ReconcileConfig
}

// NewController constructs a new controller with static RGD.
func NewController(
	log logr.Logger,
	reconcileConfig ReconcileConfig,
	gvr schema.GroupVersionResource,
	rgd *graph.Graph,
	client kroclient.SetInterface,
	labeler metadata.Labeler,
) *Controller {
	return &Controller{
		log:             log,
		client:          client,
		gvr:             gvr,
		rgd:             rgd,
		labeler:         labeler,
		reconcileConfig: reconcileConfig,
	}
}

// Reconcile implements the controller-runtime Reconcile interface.
func (c *Controller) Reconcile(ctx context.Context, req ctrl.Request) (err error) {
	log := c.log.WithValues("namespace", req.Namespace, "name", req.Name)

	//--------------------------------------------------------------
	// 1. Load instance; if gone, nothing to do
	//--------------------------------------------------------------
	inst, err := c.client.Dynamic().
		Resource(c.gvr).
		Namespace(req.Namespace).
		Get(ctx, req.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		log.Info("instance not found (likely deleted)")
		return nil
	}
	if err != nil {
		log.Error(err, "failed loading instance")
		return err
	}

	//--------------------------------------------------------------
	// 2. Create a fresh runtime for this reconciliation
	//--------------------------------------------------------------
	runtimeObj, err := c.rgd.NewGraphRuntime(inst)
	if err != nil {
		return fmt.Errorf("creation of runtime failed: %w", err)
	}

	//--------------------------------------------------------------
	// 3. Build reconciliation context (clients, mapper, labeler, runtime)
	//--------------------------------------------------------------
	rcx := NewReconcileContext(
		ctx, log, c.gvr,
		c.client.Dynamic(),
		c.client.RESTMapper(),
		c.labeler,
		runtimeObj,
		c.reconcileConfig,
		inst,
	)

	//--------------------------------------------------------------
	// 4. Handle deletion: clean up children and status
	//--------------------------------------------------------------
	if inst.GetDeletionTimestamp() != nil {
		if err := c.reconcileDeletion(rcx); err != nil {
			_ = c.updateStatus(rcx)
			return err
		}
		return c.updateStatus(rcx)
	}

	//--------------------------------------------------------------
	// 5. Ensure finalizer + management labels before mutating children
	//--------------------------------------------------------------
	if err := c.ensureManaged(rcx); err != nil {
		rcx.Mark.InstanceNotManaged("finalizer/labeling failed: %v", err)
		_ = c.updateStatus(rcx)
		return err
	}

	//--------------------------------------------------------------
	// 6. Resolve Graph (CEL, dependencies); allow data-pending
	//--------------------------------------------------------------
	if _, err := rcx.Runtime.Synchronize(); err != nil && !runtime.IsDataPending(err) {
		// Real error (not just missing data) - abort reconciliation
		rcx.Mark.GraphResolutionFailed("graph resolution failed: %v", err)
		_ = c.updateStatus(rcx)
		return err
	}
	rcx.Mark.GraphResolved()

	//--------------------------------------------------------------
	// 7. Reconcile resources (SSA + prune) and update runtime state
	//--------------------------------------------------------------
	if err := c.reconcileResources(rcx); err != nil {
		rcx.Mark.ResourcesNotReady("resource reconciliation failed: %v", err)
		_ = c.updateStatus(rcx)
		return err
	}
	rcx.Mark.ResourcesReady()

	//--------------------------------------------------------------
	// 8. Persist status/conditions
	//--------------------------------------------------------------
	return c.updateStatus(rcx)
}

func (c *Controller) ensureManaged(rcx *ReconcileContext) error {
	patched, err := c.applyManagedFinalizerAndLabels(rcx)
	if err != nil {
		return err
	}
	if patched != nil {
		rcx.Runtime.SetInstance(patched)
	}
	rcx.Mark.InstanceManaged()
	return nil
}

func (c *Controller) applyManagedFinalizerAndLabels(rcx *ReconcileContext) (*unstructured.Unstructured, error) {
	obj := rcx.Runtime.GetInstance()
	// Fast path: if everything is already correct → no patch
	hasFinalizer := metadata.HasInstanceFinalizer(obj)
	needFinalizer := !hasFinalizer

	wantLabels := c.labeler.Labels()
	haveLabels := obj.GetLabels()
	needLabelPatch := false

	for k, v := range wantLabels {
		if haveLabels[k] != v {
			needLabelPatch = true
			break
		}
	}

	if !needFinalizer && !needLabelPatch {
		return obj, nil
	}

	//-------------------------------------------
	// Build a minimal patch object (SSA apply)
	//-------------------------------------------
	patch := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": obj.GetAPIVersion(),
			"kind":       obj.GetKind(),
			"metadata": map[string]interface{}{
				"name":      obj.GetName(),
				"namespace": obj.GetNamespace(),
			},
		},
	}

	// Finalizer patch
	if needFinalizer {
		metadata.SetInstanceFinalizer(patch)
	}

	// Label patch
	if needLabelPatch {
		lbl := make(map[string]string, len(wantLabels))
		for k, v := range wantLabels {
			lbl[k] = v
		}
		patch.SetLabels(lbl)
	}

	patched, err := rcx.InstanceClient().Apply(
		rcx.Ctx,
		obj.GetName(),
		patch,
		metav1.ApplyOptions{
			FieldManager: FieldManagerForLabeler,
			Force:        true,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed applying managed finalizer/labels: %w", err)
	}

	return patched, nil
}
