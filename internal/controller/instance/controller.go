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

package instance

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/aws-controllers-k8s/symphony/internal/k8smetadata"
	"github.com/aws-controllers-k8s/symphony/internal/resourcegroup"
)

// Controller structure
type Controller struct {
	log             logr.Logger
	gvr             schema.GroupVersionResource
	client          dynamic.Interface
	rg              *resourcegroup.ResourceGroup
	instanceLabeler k8smetadata.Labeler
}

// NewController creates a new Controller instance
func NewController(
	log logr.Logger,
	gvr schema.GroupVersionResource,
	rg *resourcegroup.ResourceGroup,
	client dynamic.Interface,
	instanceLabeler k8smetadata.Labeler,
) *Controller {
	return &Controller{
		log:             log,
		gvr:             gvr,
		client:          client,
		rg:              rg,
		instanceLabeler: instanceLabeler,
	}
}

// NewGraphExecReconciler is the main reconciliation loop for the Controller
func (c *Controller) Reconcile(ctx context.Context, req ctrl.Request) error {
	namespace, name := getNamespaceName(req)

	log := c.log.WithValues("namespace", namespace, "name", name)

	instance, err := c.client.Resource(c.gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Instance not found, it may have been deleted")
			return nil
		}
		log.Error(err, "Failed to get instance")
		return nil
	}

	rgRuntime, err := c.rg.NewRuntime(instance)
	if err != nil {
		return fmt.Errorf("failed to create runtime resource group: %w", err)
	}

	instanceSubResourcesLabeler, err := k8smetadata.NewInstanceLabeler(instance).Merge(c.instanceLabeler)
	if err != nil {
		return fmt.Errorf("failed to create instance sub-resources labeler: %w", err)
	}

	graphExecReconciler := &InstanceGraphReconciler{
		log:                         log,
		gvr:                         c.gvr,
		client:                      c.client,
		rg:                          c.rg,
		runtime:                     rgRuntime,
		originalRequest:             req,
		instanceLabeler:             c.instanceLabeler,
		instanceSubResourcesLabeler: instanceSubResourcesLabeler,
	}
	return graphExecReconciler.Reconcile(ctx)
}
