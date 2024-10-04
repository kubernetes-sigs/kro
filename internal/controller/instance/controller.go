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
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/aws-controllers-k8s/symphony/internal/k8smetadata"
	"github.com/aws-controllers-k8s/symphony/internal/resourcegroup/graph"
)

// ReconcileConfig holds configuration parameters for the recnociliation process.
// It allows the customization of various aspects of the controller's behavior.
type ReconcileConfig struct {
	// DefaultRequeueDuration is the default duration to wait before requeueing a
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

// Controller manages the reconciliation of a single instance of a ResourceGroup,
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
// For each instance of a ResourceGroup, the controller creates a new instance of
// the InstanceGraphReconciler to manage the reconciliation of the instance and its
// sub-resources.
//
// It is important to state that when the controller is reconciling an instance, it
// creates and uses a new instance of the ResourceGroupRuntime to uniquely manage
// the state of the instance and its sub-resources. This ensure that at each
// reconciliation loop, the controller is working with a fresh state of the instance
// and its sub-resources.
type Controller struct {
	log logr.Logger
	// gvr represents the Group, Version, and Resource of the custom resource
	// this controller is responsible for.
	gvr schema.GroupVersionResource
	// client is a dynamic client for interacting with the Kubernetes API server.
	client dynamic.Interface
	// rg is a read-only reference to the ResourceGroup that the controller is
	// managing instances for.
	// TODO: use a read-only interface for the ResourceGroup
	rg *graph.Graph
	// instanceLabeler is responsible for applying consistent labels
	// to resources managed by this controller.
	instanceLabeler k8smetadata.Labeler
	// reconcileConfig holds the configuration parameters for the reconciliation
	// process.
	reconcileConfig ReconcileConfig
}

// NewController creates a new Controller instance.
func NewController(
	log logr.Logger,
	reconcileConfig ReconcileConfig,
	gvr schema.GroupVersionResource,
	rg *graph.Graph,
	client dynamic.Interface,
	instanceLabeler k8smetadata.Labeler,
) *Controller {
	return &Controller{
		log:             log,
		gvr:             gvr,
		client:          client,
		rg:              rg,
		instanceLabeler: instanceLabeler,
		reconcileConfig: reconcileConfig,
	}
}

// Reconcile is a handler function that reconciles the instance and its sub-resources.
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

	// This is one of the main reasons why we're splitting the controller into
	// two parts. The instanciator is responsible for creating a new runtime
	// instance of the resource group. The instance graph reconciler is responsible
	// for reconciling the instance and its sub-resources, while keeping the same
	// runtime object in it's fields.
	rgRuntime, err := c.rg.NewGraphRuntime(instance)
	if err != nil {
		return fmt.Errorf("failed to create runtime resource group: %w", err)
	}

	instanceSubResourcesLabeler, err := k8smetadata.NewInstanceLabeler(instance).Merge(c.instanceLabeler)
	if err != nil {
		return fmt.Errorf("failed to create instance sub-resources labeler: %w", err)
	}

	instanceGraphReconciler := &instanceGraphReconciler{
		log:                         log,
		gvr:                         c.gvr,
		client:                      c.client,
		runtime:                     rgRuntime,
		instanceLabeler:             c.instanceLabeler,
		instanceSubResourcesLabeler: instanceSubResourcesLabeler,
		reconcileConfig:             c.reconcileConfig,
	}
	return instanceGraphReconciler.reconcile(ctx)
}

func getNamespaceName(req ctrl.Request) (string, string) {
	parts := strings.Split(req.Name, "/")
	name := parts[len(parts)-1]
	namespace := parts[0]
	if namespace == "" {
		namespace = metav1.NamespaceDefault
	}
	return namespace, name
}
