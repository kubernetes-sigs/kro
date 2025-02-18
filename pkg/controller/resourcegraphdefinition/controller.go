// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package resourcegraphdefinition

import (
	"context"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
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
	log        logr.Logger
	rootLogger logr.Logger

	allowCRDDeletion bool

	client.Client
	clientSet  *kroclient.Set
	crdManager kroclient.CRDClient

	metadataLabeler   metadata.Labeler
	rgBuilder         *graph.Builder
	dynamicController *dynamiccontroller.DynamicController
}

func NewResourceGraphDefinitionReconciler(
	log logr.Logger,
	mgrClient client.Client,
	clientSet *kroclient.Set,
	allowCRDDeletion bool,
	dynamicController *dynamiccontroller.DynamicController,
	builder *graph.Builder,
) *ResourceGraphDefinitionReconciler {
	crdWrapper := clientSet.CRD(kroclient.CRDWrapperConfig{
		Log: log,
	})
	rgLogger := log.WithName("controller.resourceGraphDefinition")

	return &ResourceGraphDefinitionReconciler{
		rootLogger:        log,
		log:               rgLogger,
		clientSet:         clientSet,
		Client:            mgrClient,
		allowCRDDeletion:  allowCRDDeletion,
		crdManager:        crdWrapper,
		dynamicController: dynamicController,
		metadataLabeler:   metadata.NewKroMetaLabeler("0.2.1", "kro-pod"),
		rgBuilder:         builder,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceGraphDefinitionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ResourceGraphDefinition{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Complete(reconcile.AsReconciler[*v1alpha1.ResourceGraphDefinition](mgr.GetClient(), r))
}

func (r *ResourceGraphDefinitionReconciler) Reconcile(ctx context.Context, resourcegraphdefinition *v1alpha1.ResourceGraphDefinition) (ctrl.Result, error) {
	rlog := r.log.WithValues("resourcegraphdefinition", types.NamespacedName{Namespace: resourcegraphdefinition.Namespace, Name: resourcegraphdefinition.Name})
	ctx = log.IntoContext(ctx, rlog)

	if !resourcegraphdefinition.DeletionTimestamp.IsZero() {
		rlog.V(1).Info("ResourceGraphDefinition is being deleted")
		if err := r.cleanupResourceGraphDefinition(ctx, resourcegraphdefinition); err != nil {
			return ctrl.Result{}, err
		}

		rlog.V(1).Info("Setting resourcegraphdefinition as unmanaged")
		if err := r.setUnmanaged(ctx, resourcegraphdefinition); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	rlog.V(1).Info("Setting resource graph definition as managed")
	if err := r.setManaged(ctx, resourcegraphdefinition); err != nil {
		return ctrl.Result{}, err
	}

	rlog.V(1).Info("Syncing resourcegraphdefinition")
	topologicalOrder, resourcesInformation, reconcileErr := r.reconcileResourceGraphDefinition(ctx, resourcegraphdefinition)

	rlog.V(1).Info("Setting resourcegraphdefinition status")
	if err := r.setResourceGraphDefinitionStatus(ctx, resourcegraphdefinition, topologicalOrder, resourcesInformation, reconcileErr); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}
