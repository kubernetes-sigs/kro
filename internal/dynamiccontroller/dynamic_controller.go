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

// Package dynamiccontroller provides a flexible and efficient solution for
// managing multiple GroupVersionResources (GVRs) in a Kubernetes environment.
// It implements a single controller capable of dynamically handling various
// resource types concurrently, adapting to runtime changes without system restarts.
//
// Key features and design considerations:
//
//  1. Multi GVR management: It handles multiple resource types concurrently,
//     creating and managing separate workflows for each.
//
//  2. Dynamic informer management: Creates and deletes informers on the fly
//     for new resource types, allowing real time adaptation to changes in the
//     cluster.
//
//  3. Minimal disruption: Operations on one resource type do not affect
//     the performance or functionality of others.
//
//  4. Minimalism: Unlike controller-runtime, this implementation
//     is tailored specifically for Symphony's needs, avoiding unnecessary
//     dependencies and overhead.
//
//  5. Future Extensibility: It allows for future enhancements such as
//     sharding and CEL cost aware leader election, which are not readily
//     achievable with k8s.io/controller-runtime.
//
// Why not use k8s.io/controller-runtime:
//
//  1. Staticc nature: controller-runtime is optimized for statically defined
//     controllers, however Symphony requires runtime creation and management
//     of controllers for various GVRs.
//
//  2. Overhead reduction: by not including unused features like leader election
//     and certain metrics, this implementation remains minimalistic and efficient.
//
//  3. Customization: this design allows for deep customization and
//     optimization specific to Symphony's unique requirements for managing
//     multiple GVRs dynamically.
//
// This implementation aims to provide a reusable, efficient, and flexible
// solution for dynamic multi-GVR controller management in Kubernetes environments.
//
// NOTE(a-hilaly): Potentially we might open source this package for broader use cases.
package dynamiccontroller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/aws-controllers-k8s/symphony/api/v1alpha1"
	"github.com/aws-controllers-k8s/symphony/internal/graphexec"
	"github.com/aws-controllers-k8s/symphony/internal/k8smetadata"
	"github.com/aws-controllers-k8s/symphony/internal/kubernetes"
	"github.com/aws-controllers-k8s/symphony/internal/requeue"
	"github.com/aws-controllers-k8s/symphony/internal/resourcegroup"
	"github.com/aws-controllers-k8s/symphony/internal/typesystem/celextractor"
)

// Config holds the configuration for DynamicController
type Config struct {
	// Workers specifies the number of workers processing items from the queue
	Workers int
	// ResyncPeriod defines the interval at which the controller will re list
	// the resources, even if there haven't been any changes.
	ResyncPeriod time.Duration
	// QueueMaxRetries is the maximum number of retries for an item in the queue
	// will be retried before being dropped.
	//
	// NOTE(a-hilaly): I'm not very sure how useful is this, i'm trying to avoid
	// situations where reconcile errors exauhst the queue.
	QueueMaxRetries int
	// ShutdownTimeout is the maximum duration to wait for the controller to
	// gracefully shutdown. We ideally want to avoid forceful shutdowns, giving
	// the controller enough time to finish processing any pending items.
	ShutdownTimeout time.Duration
}

// DynamicController (DC) is a single controller capable of managing multiple different
// kubernetes resources (GVRs) in parallel. It can safely start watching new
// resources and stop watching others at runtime - hence the term "dynamic". This
// flexibility allows us to accept and manage various resources in a Kubernetes
// cluster without requiring restarts or pod redeployments.
//
// It is mainly inspired by native Kubernetes controllers but designed for more
// flexible and lightweight operation. DC serves as the core component of Symphony's
// dynamic resource management system. Its primary purpose is to create and manage
// "micro" controllers for custom resources defined by users at runtime (via the
// ResourceGroup CRs).
type DynamicController struct {
	config Config

	// kubeClient is the dynamic client used to create the informers
	kubeClient dynamic.Interface
	// informers is a safe map of GVR to informers. Each informer is responsible
	// for watching a specific GVR.
	informers sync.Map

	// workflowOperators is a safe map of GVR to workflow operators. Each
	// workflowOperator is responsible for managing a specific GVR.
	workflowOperators sync.Map

	// queue is the workqueue used to process items
	queue workqueue.RateLimitingInterface

	log logr.Logger
	// rootLog is a logger that is passed to the workflow operators.
	rootLog logr.Logger
	// labeler is a labeler that is passed to the workflow operators.
	labeler k8smetadata.Labeler

	// Metrics. TODO(a-hilaly): Move these to a separate package ?
	reconcileTotal    *prometheus.CounterVec
	reconcileDuration *prometheus.HistogramVec
	requeueTotal      *prometheus.CounterVec
}

// NewDynamicController creates a new DynamicController instance.
func NewDynamicController(
	log logr.Logger,
	config Config,
	kubeClient dynamic.Interface,
) *DynamicController {
	rootLog := log
	logger := log.WithName("dynamic-controller")

	dc := &DynamicController{
		config:     config,
		kubeClient: kubeClient,
		// TODO(a-hilaly): Make the queue size configurable.
		queue: workqueue.NewNamedRateLimitingQueue(workqueue.NewMaxOfRateLimiter(
			workqueue.NewItemExponentialFailureRateLimiter(200*time.Millisecond, 1000*time.Second),
			&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
		), "dynamic-controller-queue"),
		log:     logger,
		rootLog: rootLog,
		// pass version and pod id from env
		labeler: k8smetadata.NewSymphonyMetaLabeler("dev", "pod-id"),
	}

	// Initialize metrics
	dc.reconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dynamic_controller_reconcile_total",
			Help: "Total number of reconciliations per GVR",
		},
		[]string{"gvr"},
	)
	dc.reconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "dynamic_controller_reconcile_duration_seconds",
			Help:    "Duration of reconciliations per GVR",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"gvr"},
	)
	dc.requeueTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dynamic_controller_requeue_total",
			Help: "Total number of requeues per GVR and type",
		},
		[]string{"gvr", "type"},
	)

	// prometheus.MustRegister(dc.reconcileTotal, dc.reconcileDuration)
	return dc
}

// AllInformerHaveSynced checks if all registered informers have synced, returns
// true if they have.
func (dc *DynamicController) AllInformerHaveSynced() bool {
	var allSynced bool
	var informerCount int

	// Unfortunately we can't know the number of informers in advance, so we need to
	// iterate over all of them to check if they have synced.

	dc.informers.Range(func(key, value interface{}) bool {
		informerCount++
		// possibly panic if the value is not a SharedIndexInformer
		informer, ok := value.(cache.SharedIndexInformer)
		if !ok {
			dc.log.Error(nil, "Failed to cast informer", "key", key)
			allSynced = false
			return false
		}
		if !informer.HasSynced() {
			allSynced = false
			return false
		}
		return true
	})

	if informerCount == 0 {
		return true
	}
	return allSynced
}

// WaitForInformerSync waits for all informers to sync or timeout
func (dc *DynamicController) WaitForInformersSync(stopCh <-chan struct{}) bool {
	dc.log.V(1).Info("Waiting for all informers to sync")
	start := time.Now()
	defer func() {
		dc.log.V(1).Info("Finished waiting for informers to sync", "duration", time.Since(start))
	}()

	return cache.WaitForCacheSync(stopCh, dc.AllInformerHaveSynced)
}

// Run starts the DynamicController.
func (dc *DynamicController) Run(ctx context.Context) error {
	defer utilruntime.HandleCrash()
	defer dc.queue.ShutDown()

	dc.log.Info("Starting dynamic controller")
	defer dc.log.Info("Shutting down dynamic controller")

	// Wait for all informers to sync
	if !dc.WaitForInformersSync(ctx.Done()) {
		return fmt.Errorf("failed to sync informers")
	}

	// Spin up workers.
	//
	// TODO(a-hilaly): Allow for dynamic scaling of workers.
	for i := 0; i < dc.config.Workers; i++ {
		go wait.UntilWithContext(ctx, dc.worker, time.Second)
	}

	<-ctx.Done()
	return dc.gracefulShutdown(dc.config.ShutdownTimeout)
}

// worker processes items from the queue.
func (dc *DynamicController) worker(ctx context.Context) {
	for dc.processNextWorkItem(ctx) {
	}
}

// processNextWorkItem processes a single item from the queue.
func (dc *DynamicController) processNextWorkItem(ctx context.Context) bool {
	obj, shutdown := dc.queue.Get()
	if shutdown {
		return false
	}
	defer dc.queue.Done(obj)

	item, ok := obj.(ObjectIdentifiers)
	if !ok {
		dc.log.Error(fmt.Errorf("expected ObjectIdentifiers in queue but got %#v", obj), "Invalid item in queue")
		dc.queue.Forget(obj)
		return true
	}

	err := dc.syncFunc(ctx, item)
	if err == nil {
		dc.queue.Forget(obj)
		return true
	}

	gvrKey := fmt.Sprintf("%s/%s/%s", item.GVR.Group, item.GVR.Version, item.GVR.Resource)

	// Handle requeues
	switch typedErr := err.(type) {
	case *requeue.NoRequeue:
		dc.log.Error(typedErr, "Error syncing item, not requeuing", "item", item)
		dc.requeueTotal.WithLabelValues(gvrKey, "no_requeue").Inc()
		dc.queue.Forget(obj)
	case *requeue.RequeueNeeded:
		dc.log.V(1).Info("Requeue needed", "item", item, "error", typedErr)
		dc.requeueTotal.WithLabelValues(gvrKey, "requeue").Inc()
		dc.queue.Add(obj) // Add without rate limiting
	case *requeue.RequeueNeededAfter:
		dc.log.V(1).Info("Requeue needed after delay", "item", item, "error", typedErr, "delay", typedErr.Duration())
		dc.requeueTotal.WithLabelValues(gvrKey, "requeue_after").Inc()
		dc.queue.AddAfter(obj, typedErr.Duration())
	default:
		// Arriving here means we have an unexpected error, we should requeue the item
		// with rate limiting.
		dc.requeueTotal.WithLabelValues(gvrKey, "rate_limited").Inc()
		if dc.queue.NumRequeues(obj) < dc.config.QueueMaxRetries {
			dc.log.Error(err, "Error syncing item, requeuing with rate limit", "item", item)
			dc.queue.AddRateLimited(obj)
		} else {
			dc.log.Error(err, "Dropping item from queue after max retries", "item", item)
			dc.queue.Forget(obj)
		}
	}

	return true
}

// syncFunc reconciles a single item.
func (dc *DynamicController) syncFunc(ctx context.Context, oi ObjectIdentifiers) error {
	gvrKey := fmt.Sprintf("%s/%s/%s", oi.GVR.Group, oi.GVR.Version, oi.GVR.Resource)
	dc.log.V(1).Info("Syncing resourcegroup instance request", "gvr", gvrKey, "namespacedKey", oi.NamespacedKey)

	startTime := time.Now()
	defer func() {
		duration := time.Since(startTime)
		dc.reconcileDuration.WithLabelValues(gvrKey).Observe(duration.Seconds())
		dc.reconcileTotal.WithLabelValues(gvrKey).Inc()
		dc.log.V(1).Info("Finished syncing resourcegroup instance request",
			"gvr", gvrKey,
			"namespacedKey", oi.NamespacedKey,
			"duration", duration)
	}()

	wo, ok := dc.workflowOperators.Load(oi.GVR)
	if !ok {
		// NOTE(a-hilaly): this might mean that the GVR is not registered, or the workflow operator
		// is not found. We should probably handle this in a better way.
		return fmt.Errorf("no workflow operator found for GVR: %s", gvrKey)
	}

	// this is worth a panic if it fails...
	workflowOperator, ok := wo.(*graphexec.Controller)
	if !ok {
		return fmt.Errorf("invalid workflow operator type for GVR: %s", gvrKey)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: oi.NamespacedKey}}
	return workflowOperator.Reconcile(ctx, req)
}

// gracefulShutdown performs a graceful shutdown of the controller.
func (dc *DynamicController) gracefulShutdown(timeout time.Duration) error {
	dc.log.Info("Starting graceful shutdown")

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var wg sync.WaitGroup
	dc.informers.Range(func(key, value interface{}) bool {
		wg.Add(1)
		go func(informer dynamicinformer.DynamicSharedInformerFactory) {
			defer wg.Done()
			informer.Shutdown()
		}(value.(dynamicinformer.DynamicSharedInformerFactory))
		return true
	})

	// Wait for all informers to shut down or timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		dc.log.Info("All informers shut down successfully")
	case <-ctx.Done():
		dc.log.Error(ctx.Err(), "Timeout waiting for informers to shut down")
		return ctx.Err()
	}

	return nil
}

// ObjectIdentifiers is a struct that holds the namespaced key and the GVR of the object.
//
// Since we are handling all the resources using the same handlerFunc, we need to know
// what GVR we're dealing with - so that we can use the appropriate workflow operator.
type ObjectIdentifiers struct {
	// NamespacedKey is the namespaced key of the object. Typically in the format
	// `namespace/name`.
	NamespacedKey string
	GVR           schema.GroupVersionResource
}

// SafeRegisterGVK registers a new GVK to the informers map safely.
func (dc *DynamicController) SafeRegisterGVK(ctx context.Context, gvr schema.GroupVersionResource) error {
	dc.log.V(1).Info("Registering new GVK", "gvr", gvr)

	// Use Load to check if the GVK is already registered
	_, exists := dc.informers.Load(gvr)
	if exists {
		return fmt.Errorf("GVK %v already registered", gvr.String())
	}

	// Create a new informer
	gvkInformer := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dc.kubeClient,
		dc.config.ResyncPeriod,
		// Maybe we can make this configurable in the future. Thinking that
		// we might want to filter out some resources, by namespace or labels
		"",
		nil,
	)

	informer := gvkInformer.ForResource(gvr).Informer()

	// Set up event handlers
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { dc.enqueueObject(obj, "add") },
		UpdateFunc: dc.updateFunc,
		DeleteFunc: func(obj interface{}) { dc.enqueueObject(obj, "delete") },
	})
	if err != nil {
		dc.log.Error(err, "Failed to add event handler", "gvr", gvr)
		dc.informers.Delete(gvr)
		return fmt.Errorf("failed to add event handler for GVR %s: %w", gvr, err)
	}

	// Start the informer
	go func() {
		dc.log.V(1).Info("Starting informer", "gvr", gvr)
		informer.Run(ctx.Done())
	}()

	// Wait for cache sync with a timeout
	synced := cache.WaitForCacheSync(ctx.Done(), informer.HasSynced)
	if !synced {
		dc.log.Error(nil, "Failed to sync informer cache", "gvr", gvr)
		dc.informers.Delete(gvr)
		return fmt.Errorf("failed to sync informer cache for GVR %s", gvr)
	}

	dc.informers.Store(gvr, gvkInformer)
	dc.log.V(1).Info("Successfully registered GVK", "gvr", gvr)
	return nil
}

// updateFunc is the update event handler for the GVR informers
func (dc *DynamicController) updateFunc(old, new interface{}) {
	newObj, ok := new.(*unstructured.Unstructured)
	if !ok {
		dc.log.Error(nil, "failed to cast new object to unstructured")
		return
	}
	oldObj, ok := old.(*unstructured.Unstructured)
	if !ok {
		dc.log.Error(nil, "failed to cast old object to unstructured")
		return
	}

	if newObj.GetGeneration() == oldObj.GetGeneration() {
		dc.log.V(2).Info("Skipping update due to unchanged generation",
			"name", newObj.GetName(),
			"namespace", newObj.GetNamespace(),
			"generation", newObj.GetGeneration())
		return
	}

	dc.enqueueObject(new, "update")
}

// enqueueObject adds an object to the workqueue
func (dc *DynamicController) enqueueObject(obj interface{}, eventType string) {
	namespacedKey, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		dc.log.Error(err, "Failed to get key for object", "eventType", eventType)
		return
	}

	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		err := fmt.Errorf("object is not an Unstructured")
		dc.log.Error(err, "Failed to cast object to Unstructured", "eventType", eventType, "namespacedKey", namespacedKey)
		return
	}

	gvk := u.GroupVersionKind()
	gvr := k8smetadata.GVKtoGVR(gvk)

	objectIdentifiers := ObjectIdentifiers{
		NamespacedKey: namespacedKey,
		GVR:           gvr,
	}

	dc.log.V(1).Info("Enqueueing object",
		"objectIdentifiers", objectIdentifiers,
		"eventType", eventType)
	dc.queue.Add(objectIdentifiers)
}

// UnregisterGVK safely removes a GVK from the controller and cleans up associated resources.
func (dc *DynamicController) UnregisterGVK(ctx context.Context, gvr schema.GroupVersionResource) error {
	dc.log.Info("Unregistering GVK", "gvr", gvr)

	// Retrieve the informer
	informerObj, ok := dc.informers.Load(gvr)
	if !ok {
		dc.log.V(1).Info("GVK not registered, nothing to unregister", "gvr", gvr)
		return nil
	}

	informer, ok := informerObj.(dynamicinformer.DynamicSharedInformerFactory)
	if !ok {
		return fmt.Errorf("invalid informer type for GVR: %s", gvr)
	}

	// Create a context with timeout for graceful shutdown
	shutdownCtx, cancel := context.WithTimeout(ctx, dc.config.ShutdownTimeout)
	defer cancel()

	// Stop the informer
	dc.log.V(1).Info("Stopping informer", "gvr", gvr)
	informer.Shutdown()

	// Wait for the informer to stop or timeout
	select {
	case <-shutdownCtx.Done():
		if shutdownCtx.Err() == context.DeadlineExceeded {
			dc.log.Error(nil, "Timeout while waiting for informer to stop", "gvr", gvr)
			return fmt.Errorf("timeout while unregistering GVR: %s", gvr)
		}
	case <-ctx.Done():
		return ctx.Err()
	default:
		dc.log.V(1).Info("Informer stopped successfully", "gvr", gvr)
	}

	// Remove the informer from the map
	dc.informers.Delete(gvr)

	// Unregister the workflow operator
	dc.workflowOperators.Delete(gvr)

	// Clean up any pending items in the queue for this GVR
	// NOTE(a-hilaly): This is a bit heavy.. maybe we can find a better way to do this.
	// Thinking that we might want to have a queue per GVR.
	// dc.cleanupQueue(gvr)

	dc.log.Info("Successfully unregistered GVK", "gvr", gvr)
	return nil
}

// RegisterWorkflowOperator registers a new workflow operator for a ResourceGroup
func (dc *DynamicController) RegisterWorkflowOperator(ctx context.Context, rgResource *v1alpha1.ResourceGroup) (*resourcegroup.ResourceGroup, error) {
	dc.log.V(1).Info("Registering workflow operator", "resourceGroup", rgResource.Name)

	// Create a new REST config
	restConfig, err := kubernetes.NewRestConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to create REST config: %w", err)
	}

	// Create a new ResourceGroupBuilder
	builder, err := resourcegroup.NewResourceGroupBuilder(restConfig, celextractor.NewCELExpressionParser())
	if err != nil {
		return nil, fmt.Errorf("failed to create ResourceGroupBuilder: %w", err)
	}

	// Process the ResourceGroup
	processedRG, err := builder.NewResourceGroup(rgResource)
	if err != nil {
		return nil, fmt.Errorf("failed to process ResourceGroup: %w", err)
	}

	// Get the GVR for the ResourceGroup
	gvr := k8smetadata.GVKtoGVR(processedRG.Instance.GroupVersionKind)
	dc.log.V(1).Info("Creating workflow operator", "gvr", gvr)

	// Create a ResourceGroupLabeler
	rgLabeler := k8smetadata.NewResourceGroupLabeler(rgResource)

	// Merge the ResourceGroupLabeler with the SymphonyLabeler
	graphExecLabeler, err := dc.labeler.Merge(rgLabeler)
	if err != nil {
		return nil, fmt.Errorf("failed to merge labelers: %w", err)
	}

	// Create a new GraphExec controller
	wo := graphexec.New(dc.rootLog, gvr, processedRG, dc.kubeClient, graphExecLabeler)

	// Store the workflow operator
	dc.workflowOperators.Store(gvr, wo)

	// Register the GVR with the dynamic controller
	if err := dc.SafeRegisterGVK(ctx, gvr); err != nil {
		dc.workflowOperators.Delete(gvr)
		return nil, fmt.Errorf("failed to register GVK: %w", err)
	}

	dc.log.V(1).Info("Successfully registered workflow operator", "resourceGroup", rgResource.Name, "gvr", gvr)
	return processedRG, nil
}

// UnregisterWorkflowOperator unregisters a workflow operator for a given GVR
func (dc *DynamicController) UnregisterWorkflowOperator(ctx context.Context, gvr schema.GroupVersionResource) error {
	dc.log.V(1).Info("Unregistering workflow operator", "gvr", gvr)

	// Remove the workflow operator from the map
	if _, loaded := dc.workflowOperators.LoadAndDelete(gvr); !loaded {
		dc.log.V(1).Info("Workflow operator not found, nothing to unregister", "gvr", gvr)
		return nil
	}

	// Unregister the GVR from the dynamic controller
	if err := dc.UnregisterGVK(ctx, gvr); err != nil {
		return fmt.Errorf("failed to unregister GVK: %w", err)
	}

	dc.log.V(1).Info("Successfully unregistered workflow operator", "gvr", gvr)
	return nil
}
