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
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"

	kroclient "github.com/kubernetes-sigs/kro/pkg/client"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// ReconcileConfig holds configuration parameters for the reconciliation process.
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
type Controller struct {
	log logr.Logger
	// gvr represents the Group, Version, and Resource of the custom resource
	// this controller is responsible for.
	gvr schema.GroupVersionResource
	// client holds the dynamic client to use for interacting with the Kubernetes API.
	clientSet kroclient.SetInterface
	// rgd is a read-only reference to the Graph that the controller is
	// managing instances for.
	// TODO: use a read-only interface for the ResourceGraphDefinition
	rgd *graph.Graph
	// instanceLabeler is responsible for applying consistent labels
	// to resources managed by this controller.
	instanceLabeler metadata.Labeler
	// reconcileConfig holds the configuration parameters for the reconciliation
	// process.
	reconcileConfig ReconcileConfig
}

// NewController creates a new Controller instance.
func NewController(
	log logr.Logger,
	reconcileConfig ReconcileConfig,
	gvr schema.GroupVersionResource,
	rgd *graph.Graph,
	clientSet kroclient.SetInterface,
	restMapper meta.RESTMapper,
	instanceLabeler metadata.Labeler,
) *Controller {
	return &Controller{
		log:             log,
		gvr:             gvr,
		clientSet:       clientSet,
		rgd:             rgd,
		instanceLabeler: instanceLabeler,
		reconcileConfig: reconcileConfig,
	}
}

// Reconcile is a handler function that reconciles the instance and its sub-resources.
func (c *Controller) Reconcile(ctx context.Context, req ctrl.Request) error {
	log := c.log.WithValues("namespace", req.Namespace, "name", req.Name)

	instance, err := c.clientSet.Dynamic().Resource(c.gvr).Namespace(req.Namespace).Get(ctx, req.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Instance not found, it may have been deleted")
			return nil
		}
		log.Error(err, "Failed to get instance")
		return nil
	}

	// This is one of the main reasons why we're splitting the controller into
	// two parts. The instantiator is responsible for creating a new runtime
	// instance of the resource graph definition. The instance graph reconciler is responsible
	// for reconciling the instance and its sub-resources, while keeping the same
	// runtime object in it's fields.
	rgRuntime, err := c.rgd.NewGraphRuntime(instance)
	if err != nil {
		// Mark graph resolution failure and update status before returning error
		mark := NewConditionsMarkerFor(instance)
		mark.GraphNotResolved("failed to create runtime resource graph definition: %v", err)
		c.updateInstanceStatusOnError(ctx, instance)
		return fmt.Errorf("failed to create runtime resource graph definition: %w", err)
	}

	instanceSubResourcesLabeler, err := metadata.NewInstanceLabeler(instance).Merge(c.instanceLabeler)
	if err != nil {
		return fmt.Errorf("failed to create instance sub-resources labeler: %w", err)
	}

	instanceGraphReconciler := &instanceGraphReconciler{
		log:                         log,
		gvr:                         c.gvr,
		client:                      c.clientSet.Dynamic(),
		restMapper:                  c.clientSet.RESTMapper(),
		runtime:                     rgRuntime,
		instanceLabeler:             c.instanceLabeler,
		instanceSubResourcesLabeler: instanceSubResourcesLabeler,
		reconcileConfig:             c.reconcileConfig,
		// Fresh instance state at each reconciliation loop.
		state: newInstanceState(),
	}
	return instanceGraphReconciler.reconcile(ctx)
}

// updateInstanceStatusOnError updates the instance status when errors occur in the main controller
func (c *Controller) updateInstanceStatusOnError(ctx context.Context, instance *unstructured.Unstructured) {
	// Prepare status with error conditions
	wrapped := wrapInstance(instance)
	conditionSet := instanceConditionTypes.For(wrapped)

	status := map[string]interface{}{
		"state": InstanceStateError,
	}

	if conditions := conditionSet.List(); len(conditions) > 0 {
		// Marshal conditions to JSON and then unmarshal to []interface{} to get map[string]interface{} representation
		conditionsJSON, err := json.Marshal(conditions)
		if err == nil {
			var conditionsInterface []interface{}
			if err := json.Unmarshal(conditionsJSON, &conditionsInterface); err == nil {
				status["conditions"] = conditionsInterface
			}
		}
	}

	// Update status - ignore errors as this is best effort
	instance.Object["status"] = status
	_, _ = c.clientSet.Dynamic().Resource(c.gvr).
		Namespace(instance.GetNamespace()).
		UpdateStatus(ctx, instance, metav1.UpdateOptions{})
}
