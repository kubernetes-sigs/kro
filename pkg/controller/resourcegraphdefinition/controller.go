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

	"github.com/go-logr/logr"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
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

	// workloadClientSet is used for CRD operations and instance controller
	workloadClientSet kroclient.SetInterface
	// rgdCluster is for watching RGDs (may be same as manager in single-cluster mode)
	rgdCluster cluster.Cluster
	// workloadCluster is for watching CRDs (may be same as manager in single-cluster mode)
	workloadCluster cluster.Cluster
	crdManager      kroclient.CRDClient

	metadataLabeler         metadata.Labeler
	rgBuilder               *graph.Builder
	dynamicController       *dynamiccontroller.DynamicController
	maxConcurrentReconciles int
}

// NewResourceGraphDefinitionReconciler creates a new ResourceGraphDefinitionReconciler.
// workloadClientSet is used for CRD and instance operations.
// rgdCluster is used for RGD cache watching.
// workloadCluster is used for CRD cache watching.
// In single-cluster mode, both clusters can be the manager.
func NewResourceGraphDefinitionReconciler(
	workloadClientSet kroclient.SetInterface,
	rgdCluster cluster.Cluster,
	workloadCluster cluster.Cluster,
	allowCRDDeletion bool,
	dynamicController *dynamiccontroller.DynamicController,
	builder *graph.Builder,
	maxConcurrentReconciles int,
) *ResourceGraphDefinitionReconciler {
	// CRD operations happen in the workload cluster
	crdWrapper := workloadClientSet.CRD(kroclient.CRDWrapperConfig{})

	return &ResourceGraphDefinitionReconciler{
		workloadClientSet:       workloadClientSet,
		rgdCluster:              rgdCluster,
		workloadCluster:         workloadCluster,
		allowCRDDeletion:        allowCRDDeletion,
		crdManager:              crdWrapper,
		dynamicController:       dynamicController,
		metadataLabeler:         metadata.NewKROMetaLabeler(),
		rgBuilder:               builder,
		maxConcurrentReconciles: maxConcurrentReconciles,
	}
}

// SetupWithManager sets up the controller with the Manager.
// The manager is used for leader election; rgdCluster for RGD watching; workloadCluster for CRD watching.
func (r *ResourceGraphDefinitionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = r.rgdCluster.GetClient()
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

	// Watch RGDs from rgdCluster, CRDs from workloadCluster
	return ctrl.NewControllerManagedBy(mgr).
		Named("ResourceGraphDefinition").
		WithOptions(
			ctrlrtcontroller.Options{
				LogConstructor:          logConstructor,
				MaxConcurrentReconciles: r.maxConcurrentReconciles,
			},
		).
		// Watch RGDs from the RGD cluster's cache
		WatchesRawSource(
			source.Kind(r.rgdCluster.GetCache(), &v1alpha1.ResourceGraphDefinition{},
				handler.TypedEnqueueRequestsFromMapFunc(func(_ context.Context, rgd *v1alpha1.ResourceGraphDefinition) []reconcile.Request {
					return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: rgd.Name}}}
				}),
				toTypedPredicate[*v1alpha1.ResourceGraphDefinition](predicate.GenerationChangedPredicate{}),
			),
		).
		// Watch CRDs from the workload cluster's cache
		WatchesRawSource(
			source.Kind(r.workloadCluster.GetCache(), &extv1.CustomResourceDefinition{},
				handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, crd *extv1.CustomResourceDefinition) []reconcile.Request {
					return r.findRGDsForCRD(ctx, crd)
				}),
				onlyUpdatesPredicate,
			),
		).
		Complete(reconcile.AsReconciler[*v1alpha1.ResourceGraphDefinition](r.rgdCluster.GetClient(), r))
}

// findRGDsForCRD returns a list of reconcile requests for the ResourceGraphDefinition
// that owns the given CRD. It is used to trigger reconciliation when a CRD is updated.
func (r *ResourceGraphDefinitionReconciler) findRGDsForCRD(ctx context.Context, obj client.Object) []reconcile.Request {
	mobj, err := meta.Accessor(obj)
	if err != nil {
		return nil
	}

	// Check if the CRD is owned by a ResourceGraphDefinition
	if !metadata.IsKROOwned(mobj) {
		return nil
	}

	rgdName, ok := mobj.GetLabels()[metadata.ResourceGraphDefinitionNameLabel]
	if !ok {
		return nil
	}

	// Return a reconcile request for the corresponding RGD
	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Name: rgdName,
			},
		},
	}
}

func (r *ResourceGraphDefinitionReconciler) Reconcile(
	ctx context.Context,
	o *v1alpha1.ResourceGraphDefinition,
) (ctrl.Result, error) {
	if !o.DeletionTimestamp.IsZero() {
		if err := r.cleanupResourceGraphDefinition(ctx, o); err != nil {
			return ctrl.Result{}, err
		}
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

// toTypedPredicate adapts an untyped predicate to a typed predicate.
func toTypedPredicate[T client.Object](p predicate.Predicate) predicate.TypedPredicate[T] {
	return predicate.TypedFuncs[T]{
		CreateFunc: func(e event.TypedCreateEvent[T]) bool {
			return p.Create(event.CreateEvent{Object: e.Object})
		},
		UpdateFunc: func(e event.TypedUpdateEvent[T]) bool {
			return p.Update(event.UpdateEvent{ObjectOld: e.ObjectOld, ObjectNew: e.ObjectNew})
		},
		DeleteFunc: func(e event.TypedDeleteEvent[T]) bool {
			return p.Delete(event.DeleteEvent{Object: e.Object})
		},
		GenericFunc: func(e event.TypedGenericEvent[T]) bool {
			return p.Generic(event.GenericEvent{Object: e.Object})
		},
	}
}

// onlyUpdatesPredicate filters to only update events.
var onlyUpdatesPredicate = predicate.TypedFuncs[*extv1.CustomResourceDefinition]{
	UpdateFunc: func(e event.TypedUpdateEvent[*extv1.CustomResourceDefinition]) bool {
		return true
	},
	CreateFunc: func(e event.TypedCreateEvent[*extv1.CustomResourceDefinition]) bool {
		return false
	},
	DeleteFunc: func(e event.TypedDeleteEvent[*extv1.CustomResourceDefinition]) bool {
		return false
	},
}
