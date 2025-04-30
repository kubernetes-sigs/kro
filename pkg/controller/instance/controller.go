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

package instance

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kro-run/kro/api/v1alpha1"
	kroclient "github.com/kro-run/kro/pkg/client"
	"github.com/kro-run/kro/pkg/graph"
	"github.com/kro-run/kro/pkg/metadata"
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
	clientSet *kroclient.Set
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
	// defaultServiceAccounts is a map of service accounts to use for controller impersonation.
	defaultServiceAccounts map[string]string
}

// NewController creates a new Controller instance.
func NewController(
	log logr.Logger,
	reconcileConfig ReconcileConfig,
	gvr schema.GroupVersionResource,
	rgd *graph.Graph,
	clientSet *kroclient.Set,
	defaultServiceAccounts map[string]string,
	instanceLabeler metadata.Labeler,
) *Controller {
	return &Controller{
		log:                    log,
		gvr:                    gvr,
		clientSet:              clientSet,
		rgd:                    rgd,
		instanceLabeler:        instanceLabeler,
		reconcileConfig:        reconcileConfig,
		defaultServiceAccounts: defaultServiceAccounts,
	}
}

// Reconcile is a handler function that reconciles the instance and its sub-resources.
func (c *Controller) Reconcile(ctx context.Context, req ctrl.Request) error {
	namespace, name := getNamespaceName(req)

	log := c.log.WithValues("namespace", namespace, "name", name)

	instance, err := c.clientSet.Dynamic().Resource(c.gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
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
		return fmt.Errorf("failed to create runtime resource graph definition: %w", err)
	}

	instanceSubResourcesLabeler, err := metadata.NewInstanceLabeler(instance).Merge(c.instanceLabeler)
	if err != nil {
		return fmt.Errorf("failed to create instance sub-resources labeler: %w", err)
	}

	// If possible, use a service account to create the execution client
	// TODO(a-hilaly): client caching
	executionClient, err := c.getExecutionClient(namespace)
	if err != nil {
		return fmt.Errorf("failed to create execution client: %w", err)
	}

	instanceGraphReconciler := &instanceGraphReconciler{
		log:                         log,
		gvr:                         c.gvr,
		client:                      executionClient,
		runtime:                     rgRuntime,
		instanceLabeler:             c.instanceLabeler,
		instanceSubResourcesLabeler: instanceSubResourcesLabeler,
		reconcileConfig:             c.reconcileConfig,
		// Fresh instance state at each reconciliation loop.
		state: newInstanceState(),
	}
	return instanceGraphReconciler.reconcile(ctx)
}

// getNamespaceName extracts the namespace and name from the request.
func getNamespaceName(req ctrl.Request) (string, string) {
	parts := strings.Split(req.Name, "/")
	name := parts[len(parts)-1]
	namespace := parts[0]
	if namespace == "" {
		namespace = metav1.NamespaceDefault
	}
	return namespace, name
}

// errorCategory helps classify different types of impersonation errors
type errorCategory string

const (
	errorConfigCreate errorCategory = "config_create"
	errorInvalidSA    errorCategory = "invalid_sa"
	errorClientCreate errorCategory = "client_create"
	errorPermissions  errorCategory = "permissions"
)

// getExecutionClient determines the execution client to use for the instance.
// If the instance is created in a namespace of which a service account is specified,
// the execution client will be created using the service account. If no service account
// is specified for the namespace, the default client will be used.
func (c *Controller) getExecutionClient(namespace string) (dynamic.Interface, error) {
	// if no service accounts are specified, use the default client
	if len(c.defaultServiceAccounts) == 0 {
		c.log.V(1).Info("no service accounts configured, using default client")
		return c.clientSet.Dynamic(), nil
	}

	timer := prometheus.NewTimer(impersonationDuration.WithLabelValues(namespace, ""))
	defer timer.ObserveDuration()

	// Check for namespace specific service account
	if sa, ok := c.defaultServiceAccounts[namespace]; ok {
		userName, err := getServiceAccountUserName(namespace, sa)
		if err != nil {
			c.handleImpersonateError(namespace, sa, err)
			return nil, fmt.Errorf("invalid service account configuration: %w", err)
		}

		pivotedClient, err := c.clientSet.WithImpersonation(userName)
		if err != nil {
			c.handleImpersonateError(namespace, sa, err)
			return nil, fmt.Errorf("failed to create impersonated client: %w", err)
		}

		impersonationTotal.WithLabelValues(namespace, sa, "success").Inc()
		return pivotedClient.Dynamic(), nil
	}

	// Check for default service account (marked by "*")
	if defaultSA, ok := c.defaultServiceAccounts[v1alpha1.DefaultServiceAccountKey]; ok {
		userName, err := getServiceAccountUserName(namespace, defaultSA)
		if err != nil {
			c.handleImpersonateError(namespace, defaultSA, err)
			return nil, fmt.Errorf("invalid default service account configuration: %w", err)
		}

		pivotedClient, err := c.clientSet.WithImpersonation(userName)
		if err != nil {
			c.handleImpersonateError(namespace, defaultSA, err)
			return nil, fmt.Errorf("failed to create impersonated client with default SA: %w", err)
		}

		impersonationTotal.WithLabelValues(namespace, defaultSA, "success").Inc()
		return pivotedClient.Dynamic(), nil
	}

	impersonationTotal.WithLabelValues(namespace, "", "default").Inc()
	// Fallback to the default client
	return c.clientSet.Dynamic(), nil
}

// handleImpersonateError logs the error and records the error in the metrics
func (c *Controller) handleImpersonateError(namespace, sa string, err error) {
	var category errorCategory
	switch {
	case strings.Contains(err.Error(), "forbidden"):
		category = errorPermissions
	case strings.Contains(err.Error(), "cannot get token"):
		category = errorConfigCreate
	default:
		category = errorClientCreate
	}
	recordImpersonateError(namespace, sa, category)
	c.log.Error(
		err,
		"failed to create impersonated client",
		"namespace", namespace,
		"serviceAccount", sa,
		"errorCategory", category,
	)
}

// getServiceAccountUserName builds the impersonate service account user name.
// The format of the user name is "system:serviceaccount:<namespace>:<serviceaccount>"
func getServiceAccountUserName(namespace, serviceAccount string) (string, error) {
	if namespace == "" || serviceAccount == "" {
		return "", fmt.Errorf("namespace and service account must be provided")
	}
	return fmt.Sprintf("system:serviceaccount:%s:%s", namespace, serviceAccount), nil
}
