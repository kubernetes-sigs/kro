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

package resourcegraphdefinition

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlrtcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	kroclient "github.com/kubernetes-sigs/kro/pkg/client"
	"github.com/kubernetes-sigs/kro/pkg/dynamiccontroller"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// ResourceGraphDefinitionReconciler reconciles a ResourceGraphDefinition object
type ResourceGraphDefinitionReconciler struct {
	allowCRDDeletion bool

	// Client and instanceLogger are set with SetupWithManager

	client.Client

	instanceLogger logr.Logger

	clientSet kroclient.SetInterface
	crdClient apiextensionsv1.CustomResourceDefinitionInterface

	metadataLabeler         metadata.Labeler
	rgBuilder               *graph.Builder
	dynamicController       *dynamiccontroller.DynamicController
	maxConcurrentReconciles int

	// crdInformer is our own informer for CRDs to reliably detect establishment
	crdInformer *CRDInformer
}

func NewResourceGraphDefinitionReconciler(
	clientSet kroclient.SetInterface,
	allowCRDDeletion bool,
	dynamicController *dynamiccontroller.DynamicController,
	builder *graph.Builder,
	maxConcurrentReconciles int,
) *ResourceGraphDefinitionReconciler {
	return &ResourceGraphDefinitionReconciler{
		clientSet:               clientSet,
		allowCRDDeletion:        allowCRDDeletion,
		crdClient:               clientSet.CRD(),
		dynamicController:       dynamicController,
		metadataLabeler:         metadata.NewKROMetaLabeler(),
		rgBuilder:               builder,
		maxConcurrentReconciles: maxConcurrentReconciles,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceGraphDefinitionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	r.clientSet.SetRESTMapper(mgr.GetRESTMapper())
	r.instanceLogger = mgr.GetLogger()

	logConstructor := func(req *reconcile.Request) logr.Logger {
		log := mgr.GetLogger().WithName("rgd-controller").WithValues(
			"controller", "ResourceGraphDefinition",
			"controllerGroup", v1alpha1.GroupVersion.Group,
			"controllerKind", "ResourceGraphDefinition",
		)
		if req != nil {
			log = log.WithValues("name", req.Name)
		}
		return log
	}

	// Create channel for external events (watch state + CRD events)
	externalEventsChan := make(chan event.GenericEvent)

	// Start goroutine to forward watch state events
	go r.watchStateEvents(externalEventsChan)

	// Create and start our own CRD informer for reliable establishment detection
	r.crdInformer = NewCRDInformer(
		mgr.GetLogger(),
		r.clientSet,
		externalEventsChan,
		0, // No resync - we only care about real events
	)
	r.crdInformer.Start()

	return ctrl.NewControllerManagedBy(mgr).
		Named("ResourceGraphDefinition").
		For(&v1alpha1.ResourceGraphDefinition{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		WithOptions(
			ctrlrtcontroller.Options{
				LogConstructor:          logConstructor,
				MaxConcurrentReconciles: r.maxConcurrentReconciles,
			},
		).
		WatchesRawSource(
			source.Channel(externalEventsChan, &handler.EnqueueRequestForObject{}),
		).
		Complete(reconcile.AsReconciler[*v1alpha1.ResourceGraphDefinition](mgr.GetClient(), r))
}

// watchStateEvents listens for watch state change events and triggers RGD reconciliation.
func (r *ResourceGraphDefinitionReconciler) watchStateEvents(eventsChan chan<- event.GenericEvent) {
	for stateEvent := range r.dynamicController.WatchStateEvents() {
		r.enqueueRGDForGVR(stateEvent.GVR, eventsChan)
	}
}

// enqueueRGDForGVR finds the RGD that owns the CRD for the given GVR and enqueues a reconcile event.
func (r *ResourceGraphDefinitionReconciler) enqueueRGDForGVR(gvr schema.GroupVersionResource, eventsChan chan<- event.GenericEvent) {
	crdName := fmt.Sprintf("%s.%s", gvr.Resource, gvr.Group)

	crd := &metav1.PartialObjectMetadata{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "apiextensions.k8s.io",
		Version: "v1",
		Kind:    "CustomResourceDefinition",
	})
	if err := r.Client.Get(context.Background(), client.ObjectKey{Name: crdName}, crd); err != nil {
		return
	}

	if !metadata.IsKROOwned(crd) {
		return
	}
	rgdName, ok := crd.GetLabels()[metadata.ResourceGraphDefinitionNameLabel]
	if !ok {
		return
	}

	rgd := &v1alpha1.ResourceGraphDefinition{}
	rgd.SetName(rgdName)

	eventsChan <- event.GenericEvent{Object: rgd}
}

func (r *ResourceGraphDefinitionReconciler) Reconcile(
	ctx context.Context,
	o *v1alpha1.ResourceGraphDefinition,
) (ctrl.Result, error) {
	if !o.DeletionTimestamp.IsZero() {
		cleanupComplete, err := r.cleanupResourceGraphDefinition(ctx, o)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !cleanupComplete {
			// Waiting for microcontroller to stop - Stopped event will trigger re-reconcile
			return ctrl.Result{}, nil
		}
		// Cleanup complete - remove finalizer
		if err := r.setUnmanaged(ctx, o); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if err := r.setManaged(ctx, o); err != nil {
		return ctrl.Result{}, err
	}

	topologicalOrder, resourcesInformation, reconcileErr := r.reconcileResourceGraphDefinition(ctx, o)

	if err := r.updateStatus(ctx, o, topologicalOrder, resourcesInformation); err != nil {
		reconcileErr = errors.Join(reconcileErr, err)
	}

	return ctrl.Result{}, reconcileErr
}
