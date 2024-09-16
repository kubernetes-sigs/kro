// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package graphexec

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/aws-controllers-k8s/symphony/api/v1alpha1"
	"github.com/aws-controllers-k8s/symphony/internal/k8smetadata"
	"github.com/aws-controllers-k8s/symphony/internal/requeue"
	"github.com/aws-controllers-k8s/symphony/internal/resourcegroup"
)

const (
	defaultNamespace = "default"
	defaultFinalizer = "finalizer.symphony.aws"
)

func New(
	log logr.Logger,
	target schema.GroupVersionResource,
	rg *resourcegroup.ResourceGroup,
	client *dynamic.DynamicClient,
	instanceLabeler k8smetadata.Labeler,
) *Controller {
	id := fmt.Sprintf("%s/%s/%s", target.Group, target.Version, target.Resource)
	log = log.WithName("controller." + id)

	return &Controller{
		id:              id,
		log:             log,
		target:          target,
		client:          client,
		rg:              rg,
		instanceLabeler: instanceLabeler,
	}
}

// Controller is the main controller responsible for reconciling a instance.
type Controller struct {
	mu                          sync.Mutex
	id                          string
	log                         logr.Logger
	target                      schema.GroupVersionResource
	client                      *dynamic.DynamicClient
	rg                          *resourcegroup.ResourceGroup
	runtime                     *resourcegroup.RuntimeResourceGroup
	instanceLabeler             k8smetadata.Labeler
	instanceSubResourcesLabeler k8smetadata.Labeler
}

// Reconcile reconciles the instance.
func (c *Controller) Reconcile(ctx context.Context, req ctrl.Request) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	log := c.log.WithValues("namespace", req.Namespace, "name", req.Name)

	namespace, name := getNamespaceName(req)
	instance, err := c.client.Resource(c.target).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		log.Info("Failed to get instance", "error", err)
		return err
	}

	rgRuntime, err := c.rg.NewRuntime(instance)
	if err != nil {
		return err
	}
	instanceSubResourcesLabeler, err := k8smetadata.NewInstanceLabeler(instance).Merge(c.instanceLabeler)
	if err != nil {
		return err
	}

	controller := &Controller{
		id:                          c.id,
		log:                         log,
		target:                      c.target,
		client:                      c.client,
		rg:                          c.rg,
		runtime:                     rgRuntime,
		instanceLabeler:             c.instanceLabeler,
		instanceSubResourcesLabeler: instanceSubResourcesLabeler,
	}

	// Resolve static variables e.g all the ${spec.xxx} variables
	log.Info("Propagating instance variables to resources (spec fields)")
	err = rgRuntime.ResolveStaticVariables()
	if err != nil {
		return controller.patchStatusError(ctx, err)
	}

	// handle deletion case
	if !instance.GetDeletionTimestamp().IsZero() {
		log.V(1).Info("Handling instance deletion", "deletionTimestamp", instance.GetDeletionTimestamp())
		return controller.handleInstanceDeletion(ctx, namespace)
	}

	log.V(1).Info("Reconciling instance", "instance", instance)
	return controller.reconcile(ctx)
}

// reconcile the instance creation or update
func (c *Controller) reconcile(ctx context.Context) error {
	instanceUnstructured := c.runtime.Instance

	patched, err := c.setManaged(ctx, instanceUnstructured, instanceUnstructured.GetUID())
	if err != nil {
		return c.patchStatusError(ctx, err)
	}

	if patched != nil {
		c.runtime.Instance.Object = patched.Object
	}

	/* // defer resolve instance status to the end of the reconcile
	defer func() {
		err := c.runtime.ResolveDynamicVariables()
		if err != nil {
			c.log.V(1).Info("Failed to resolve dynamic variables", "error", err)
			return
		}
		err = c.runtime.ResolveInstanceStatus()
		if err != nil {
			c.log.V(1).Info("Failed to resolve instance status", "error", err)
			return
		}
		fmt.Println("Instance:<<", c.runtime.Instance.Object["status"])
		if c.runtime.Instance.Object["status"] == nil {
			return
		}

		// patch the status
		err = c.patchInstanceStatus(ctx, "", nil, c.runtime.Instance.Object["status"].(map[string]interface{}))
		if err != nil {
			c.log.V(1).Info("    <><><>Failed to patch instance status   zzzzzzzzzzzzzz", "error", err)
		}
	}() */

	c.log.V(1).Info("Reconciling individual resources [following topological order]")

	for _, resourceID := range c.runtime.TopologicalOrder {
		c.log.V(1).Info("Reconciling resource", "resource", resourceID)

		resourceMeta := c.rg.Resources[resourceID]
		if !c.runtime.CanResolveResource(resourceID) {
			c.log.V(1).Info("Resource dependencies not ready", "resource", resourceID)
			return requeue.NeededAfter(fmt.Errorf("resource dependencies not ready"), 3*time.Second)
		}

		gvk := resourceMeta.GroupVersionKind
		err := c.runtime.ResolveResource(resourceID)
		if err != nil {
			return c.patchStatusError(ctx, err)
		}

		rUnstructured := c.runtime.Resources[resourceID]

		rname := rUnstructured.GetName()
		namespace := rUnstructured.GetNamespace()
		if namespace == "" {
			namespace = "default"
		}

		gvr := k8smetadata.GVKtoGVR(gvk)
		rc := c.client.Resource(gvr).Namespace(namespace)

		// Check if resource exists
		c.log.V(1).Info("Checking if resource exists", "resource", resourceID)
		observed, err := rc.Get(ctx, rname, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				c.log.V(1).Info("Resource not found, creating", "resource", resourceID)
				c.instanceSubResourcesLabeler.ApplyLabels(rUnstructured)

				_, err := rc.Create(ctx, rUnstructured, metav1.CreateOptions{})
				if err != nil {
					c.log.V(1).Error(err, "Failed to create resource", "resource", resourceID)
					return err
				}

				c.log.V(1).Info("Resource created", "resource", resourceID)
				msg := "Reconciliation in progress"
				err = c.patchInstanceStatus(ctx, "IN PROGRESS", []*v1alpha1.Condition{
					&v1alpha1.Condition{
						Type:               v1alpha1.ConditionType("AllResourcesSynced"),
						Status:             corev1.ConditionFalse,
						LastTransitionTime: &metav1.Time{Time: time.Now()},
						Reason:             &msg,
						Message:            &msg,
					},
				}, nil)
				if err != nil {
					return err
				}
				return requeue.NeededAfter(fmt.Errorf("resource created"), 3*time.Second)
			} else {
				c.log.V(1).Info("Failed to get resource", "error", err)
				return err
			}
		}

		c.log.V(1).Info("Resource exists", "resource", resourceID)
		c.runtime.SetLatestResource(resourceID, observed)
		err = c.runtime.ResolveDynamicVariables()
		if err != nil {
			return c.patchStatusError(ctx, err)
		}
	}

	c.log.V(1).Info("All resources are ready")
	c.log.V(1).Info("Setting instance state to SUCCESS")
	msg := "All resources are ready"

	err = c.runtime.ResolveDynamicVariables()
	if err != nil {
		c.log.V(1).Info("Failed to resolve dynamic variables", "error", err)
	}
	err = c.runtime.ResolveInstanceStatus()
	if err != nil {
		c.log.V(1).Info("Failed to resolve instance status", "error", err)
	}

	extra := map[string]interface{}{}
	if c.runtime.Instance.Object["status"] != nil {
		extra = c.runtime.Instance.Object["status"].(map[string]interface{})
	}
	err = c.patchInstanceStatus(ctx, "SUCCESS", []*v1alpha1.Condition{
		&v1alpha1.Condition{
			Type:               v1alpha1.ConditionType("AllResourcesSynced"),
			Status:             corev1.ConditionTrue,
			LastTransitionTime: &metav1.Time{Time: time.Now()},
			Reason:             &msg,
			Message:            &msg,
		},
	}, extra)
	if err != nil {
		return err
	}
	return nil
}

// handleInstanceDeletion deletes all resources in the graph
func (c *Controller) handleInstanceDeletion(ctx context.Context, namespace string) error {

	c.log.V(1).Info("Handling instance deletion")

	created := []string{}
	createdNamespacedNames := []struct{ name, namespace string }{}

	// First we need to get all the resources we created if any
	// and delete them in reverse order
	c.log.V(1).Info("Getting all resources created by Symphony")
	for _, resourceID := range c.runtime.TopologicalOrder {
		_ = c.runtime.ResolveDynamicVariables()

		resourceMeta := c.rg.Resources[resourceID]
		if !c.runtime.CanResolveResource(resourceID) {
			break
		}

		gvk := resourceMeta.GroupVersionKind
		gvr := k8smetadata.GVKtoGVR(gvk)
		err := c.runtime.ResolveResource(resourceID)
		if err != nil {
			return c.patchStatusError(ctx, err)
		}

		rUnstructured := c.runtime.Resources[resourceID]

		rname := rUnstructured.GetName()
		namespace := rUnstructured.GetNamespace()
		if namespace == "" {
			namespace = "default"
		}

		rc := c.client.Resource(gvr).Namespace(namespace)
		// Check if resource exists
		c.log.V(1).Info("Checking if resource exists", "resource", resourceID)
		latest, err := rc.Get(ctx, rname, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				c.log.V(1).Info("Resource not found", "resource", resourceID)
				continue
			}
			c.log.V(1).Info("Failed to get resource", "error", err)
			return err
		}

		created = append(created, resourceID)
		createdNamespacedNames = append(createdNamespacedNames, struct{ name, namespace string }{rname, namespace})
		c.runtime.SetLatestResource(resourceID, latest)
		err = c.runtime.ResolveDynamicVariables()
	}

	if len(created) == 0 {
		c.log.V(1).Info("No resources created by Symphony")
		instanceUnstructured := c.runtime.Instance
		_, err := c.setUnmanaged(ctx, instanceUnstructured, instanceUnstructured.GetUID())
		if err != nil {
			return c.patchStatusError(ctx, err)
		}
		return nil
	}

	c.log.V(1).Info("Deleting resources in reverse topological order")
	// walk resources in reverse order and delete them
	for i := len(created) - 1; i >= 0; i-- {
		resourceID := created[i]
		resourceMeta := c.rg.Resources[resourceID]

		gvk := resourceMeta.GroupVersionKind
		gvr := k8smetadata.GVKtoGVR(gvk)
		name := createdNamespacedNames[i].name
		namespace := createdNamespacedNames[i].namespace

		rc := c.client.Resource(gvr).Namespace(namespace)

		c.log.V(1).Info("Deleting resource", "resource", resourceID)
		err := rc.Delete(ctx, name, metav1.DeleteOptions{
			GracePeriodSeconds: pointer.Int64Ptr(0),
		})
		if err != nil && !apierrors.IsNotFound(err) {
			c.log.V(1).Info("Failed to delete resource", "error", err)
			return err
		}
		time.Sleep(3 * time.Second)
	}
	instanceUnstructured := c.runtime.Instance
	_, err := c.setUnmanaged(ctx, instanceUnstructured, instanceUnstructured.GetUID())
	if err != nil {
		return c.patchStatusError(ctx, err)
	}
	return nil
}

func getNamespaceName(req ctrl.Request) (string, string) {
	parts := strings.Split(req.Name, "/")
	name := parts[len(parts)-1]
	namespace := parts[0]
	if namespace == "" {
		namespace = defaultNamespace
	}
	return namespace, name
}
