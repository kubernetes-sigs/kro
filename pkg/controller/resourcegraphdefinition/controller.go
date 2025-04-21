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

package resourcegraphdefinition

import (
	"context"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlrtcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kro-run/kro/api/v1alpha1"
	kroclient "github.com/kro-run/kro/pkg/client"
	"github.com/kro-run/kro/pkg/dynamiccontroller"
	"github.com/kro-run/kro/pkg/graph"
	"github.com/kro-run/kro/pkg/metadata"
)

//+kubebuilder:rbac:groups=kro.run,resources=resourcegraphdefinitions,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kro.run,resources=resourcegraphdefinitions/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kro.run,resources=resourcegraphdefinitions/finalizers,verbs=update

// ResourceGraphDefinitionReconciler reconciles a ResourceGraphDefinition object
type ResourceGraphDefinitionReconciler struct {
	allowCRDDeletion bool

	// Client and instanceLogger are set with SetupWithManager

	client.Client
	instanceLogger logr.Logger

	clientSet  *kroclient.Set
	crdManager kroclient.CRDClient

	metadataLabeler         metadata.Labeler
	rgBuilder               *graph.Builder
	dynamicController       *dynamiccontroller.DynamicController
	maxConcurrentReconciles int
}

func NewResourceGraphDefinitionReconciler(
	clientSet *kroclient.Set,
	allowCRDDeletion bool,
	dynamicController *dynamiccontroller.DynamicController,
	builder *graph.Builder,
	maxConcurrentReconciles int,
) *ResourceGraphDefinitionReconciler {
	crdWrapper := clientSet.CRD(kroclient.CRDWrapperConfig{})

	return &ResourceGraphDefinitionReconciler{
		clientSet:               clientSet,
		allowCRDDeletion:        allowCRDDeletion,
		crdManager:              crdWrapper,
		dynamicController:       dynamicController,
		metadataLabeler:         metadata.NewKROMetaLabeler(),
		rgBuilder:               builder,
		maxConcurrentReconciles: maxConcurrentReconciles,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceGraphDefinitionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
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
		Complete(reconcile.AsReconciler[*v1alpha1.ResourceGraphDefinition](mgr.GetClient(), r))
}

func (r *ResourceGraphDefinitionReconciler) Reconcile(ctx context.Context, o *v1alpha1.ResourceGraphDefinition) (ctrl.Result, error) {
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

	return ctrl.Result{},
		r.setResourceGraphDefinitionStatus(ctx, o, topologicalOrder, resourcesInformation, reconcileErr)
}
