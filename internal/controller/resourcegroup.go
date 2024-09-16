// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package controller

import (
	"context"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/aws-controllers-k8s/symphony/api/v1alpha1"
	"github.com/aws-controllers-k8s/symphony/internal/crd"
	"github.com/aws-controllers-k8s/symphony/internal/dynamiccontroller"
)

func NewResourceGroupReconciler(
	log logr.Logger,
	mgr ctrl.Manager,
	allowCRDDeletion bool,
	crdManager *crd.Manager,
	dynamicController *dynamiccontroller.DynamicController,
) *ResourceGroupReconciler {
	log = log.WithName("controller.resourceGroup")
	return &ResourceGroupReconciler{
		log:               log,
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		AllowCRDDeletion:  allowCRDDeletion,
		CRDManager:        crdManager,
		DynamicController: dynamicController,
	}
}

// ResourceGroupReconciler reconciles a ResourceGroup object
type ResourceGroupReconciler struct {
	log logr.Logger
	client.Client
	AllowCRDDeletion  bool
	Scheme            *runtime.Scheme
	CRDManager        *crd.Manager
	DynamicController *dynamiccontroller.DynamicController
}

//+kubebuilder:rbac:groups=x.symphony.k8s.aws,resources=resourcegroups,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=x.symphony.k8s.aws,resources=resourcegroups/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=x.symphony.k8s.aws,resources=resourcegroups/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ResourceGroup object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.16.3/pkg/reconcile
func (r *ResourceGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	rlog := r.log.WithValues("resourcegroup", req.NamespacedName)
	ctx = log.IntoContext(ctx, rlog)

	var resourcegroup v1alpha1.ResourceGroup
	err := r.Get(ctx, req.NamespacedName, &resourcegroup)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// reconcile resourcegroup fiesta
	err = r.reconcile(ctx, &resourcegroup)
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ResourceGroup{}).
		Complete(r)
}
