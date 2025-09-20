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
//     is tailored specifically for kro's needs, avoiding unnecessary
//     dependencies and overhead.
//
//  5. Future Extensibility: It allows for future enhancements such as
//     sharding and CEL cost aware leader election, which are not readily
//     achievable with k8s.io/controller-runtime.
//
// Why not use k8s.io/controller-runtime:
//
//  1. Static nature: controller-runtime is optimized for statically defined
//     controllers, however kro requires runtime creation and management
//     of controllers for various GVRs.
//
//  2. Overhead reduction: by not including unused features like leader election
//     and certain metrics, this implementation remains minimalistic and efficient.
//
//  3. Customization: this design allows for deep customization and
//     optimization specific to kro's unique requirements for managing
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
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/time/rate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	k8smetadata "k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kro-run/kro/pkg/metadata"
	"github.com/kro-run/kro/pkg/requeue"
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
	// situations where reconcile errors exhaust the queue.
	QueueMaxRetries int
	// ShutdownTimeout is the maximum duration to wait for the controller to
	// gracefully shutdown. We ideally want to avoid forceful shutdowns, giving
	// the controller enough time to finish processing any pending items.
	ShutdownTimeout time.Duration
	// MinRetryDelay is the minimum delay before retrying an item in the queue
	MinRetryDelay time.Duration
	// MaxRetryDelay is the maximum delay before retrying an item in the queue
	MaxRetryDelay time.Duration
	// RateLimit is the maximum number of events processed per second
	RateLimit int
	// BurstLimit is the maximum number of events in a burst
	BurstLimit int
}

// DynamicController (DC) is a single controller capable of managing multiple different
// kubernetes resources (GVRs) in parallel. It can safely start watching new
// resources and stop watching others at runtime - hence the term "dynamic". This
// flexibility allows us to accept and manage various resources in a Kubernetes
// cluster without requiring restarts or pod redeployments.
//
// It is mainly inspired by native Kubernetes controllers but designed for more
// flexible and lightweight operation. DC serves as the core component of kro's
// dynamic resource management system. Its primary purpose is to create and manage
// "micro" controllers for custom resources defined by users at runtime (via the
// ResourceGraphDefinition CRs).
type DynamicController struct {
	config Config

	// kubeClient is the dynamic client used to create the factories
	kubeClient k8smetadata.Interface
	// factories is a safe map of GVR to factories. Each factory is responsible
	// for watching a specific GVR.
	factories sync.Map

	// handlers is a safe map of GVR to workflow operators. Each
	// handler is responsible for managing a specific GVR.
	handlers sync.Map

	// queue is the workqueue used to process items
	queue workqueue.TypedRateLimitingInterface[ObjectIdentifiers]

	log logr.Logger
}

type Handler func(ctx context.Context, req ctrl.Request) error

type factoryWrapper struct {
	factory       metadatainformer.SharedInformerFactory
	shutdown      func()
	ctx           context.Context
	registrations map[schema.GroupVersionResource]cache.ResourceEventHandlerRegistration
}

// NewDynamicController creates a new DynamicController instance.
func NewDynamicController(
	log logr.Logger,
	config Config,
	kubeClient k8smetadata.Interface) *DynamicController {
	logger := log.WithName("dynamic-controller")

	dc := &DynamicController{
		config:     config,
		kubeClient: kubeClient,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.NewTypedMaxOfRateLimiter(
			workqueue.NewTypedItemExponentialFailureRateLimiter[ObjectIdentifiers](config.MinRetryDelay, config.MaxRetryDelay),
			&workqueue.TypedBucketRateLimiter[ObjectIdentifiers]{Limiter: rate.NewLimiter(rate.Limit(config.RateLimit), config.BurstLimit)},
		), workqueue.TypedRateLimitingQueueConfig[ObjectIdentifiers]{Name: "dynamic-controller-queue"}),
		log: logger,
		// pass version and pod id from env
	}

	return dc
}

// AllInformerHaveSynced checks if all registered factories have synced, returns
// true if they have.
func (dc *DynamicController) AllInformerHaveSynced() bool {
	var allSynced bool
	var informerCount int

	// Unfortunately we can't know the number of factories in advance, so we need to
	// iterate over all of them to check if they have synced.

	dc.factories.Range(func(key, value interface{}) bool {
		informerCount++
		// possibly panic if the value is not a SharedIndexInformer
		informer, ok := value.(cache.SharedIndexInformer)
		if !ok {
			dc.log.Error(nil, "Failed to cast factory", "key", key)
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

// WaitForInformerSync waits for all factories to sync or timeout
func (dc *DynamicController) WaitForInformersSync(stopCh <-chan struct{}) bool {
	dc.log.V(1).Info("Waiting for all factories to sync")
	start := time.Now()
	defer func() {
		dc.log.V(1).Info("Finished waiting for factories to sync", "duration", time.Since(start))
	}()

	return cache.WaitForCacheSync(stopCh, dc.AllInformerHaveSynced)
}

// Run starts the DynamicController.
func (dc *DynamicController) Run(ctx context.Context) error {
	defer utilruntime.HandleCrash()

	dc.log.Info("Starting dynamic controller")
	defer dc.log.Info("Shutting down dynamic controller")

	// Wait for all factories to sync
	if !dc.WaitForInformersSync(ctx.Done()) {
		return fmt.Errorf("failed to sync factories")
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
	item, shutdown := dc.queue.Get()
	if shutdown {
		return false
	}
	defer dc.queue.Done(item)

	queueLength.Set(float64(dc.queue.Len()))

	err := dc.syncFunc(ctx, item)
	if err == nil || apierrors.IsNotFound(err) {
		dc.queue.Forget(item)
		return true
	}

	gvrKey := fmt.Sprintf("%s/%s/%s", item.GVR.Group, item.GVR.Version, item.GVR.Resource)

	// Handle requeues
	switch typedErr := err.(type) {
	case *requeue.NoRequeue:
		dc.log.Error(typedErr, "Error syncing item, not requeuing", "item", item)
		requeueTotal.WithLabelValues(gvrKey, "no_requeue").Inc()
		dc.queue.Forget(item)
	case *requeue.RequeueNeeded:
		dc.log.V(1).Info("Requeue needed", "item", item, "error", typedErr)
		requeueTotal.WithLabelValues(gvrKey, "requeue").Inc()
		dc.queue.Add(item) // Add without rate limiting
	case *requeue.RequeueNeededAfter:
		dc.log.V(1).Info("Requeue needed after delay", "item", item, "error", typedErr, "delay", typedErr.Duration())
		requeueTotal.WithLabelValues(gvrKey, "requeue_after").Inc()
		dc.queue.AddAfter(item, typedErr.Duration())
	default:
		// Arriving here means we have an unexpected error, we should requeue the item
		// with rate limiting.
		requeueTotal.WithLabelValues(gvrKey, "rate_limited").Inc()
		if dc.queue.NumRequeues(item) < dc.config.QueueMaxRetries {
			dc.log.Error(err, "Error syncing item, requeuing with rate limit", "item", item)
			dc.queue.AddRateLimited(item)
		} else {
			dc.log.Error(err, "Dropping item from queue after max retries", "item", item)
			dc.queue.Forget(item)
		}
	}

	return true
}

// syncFunc reconciles a single item.
func (dc *DynamicController) syncFunc(ctx context.Context, oi ObjectIdentifiers) error {
	gvrKey := fmt.Sprintf("%s/%s/%s", oi.GVR.Group, oi.GVR.Version, oi.GVR.Resource)
	dc.log.V(1).Info("Syncing resourcegraphdefinition instance request", "gvr", gvrKey, "namespacedKey", oi.NamespacedKey)

	startTime := time.Now()
	defer func() {
		duration := time.Since(startTime)
		reconcileDuration.WithLabelValues(gvrKey).Observe(duration.Seconds())
		reconcileTotal.WithLabelValues(gvrKey).Inc()
		dc.log.V(1).Info("Finished syncing resourcegraphdefinition instance request",
			"gvr", gvrKey,
			"namespacedKey", oi.NamespacedKey,
			"duration", duration)
	}()

	genericHandler, ok := dc.handlers.Load(oi.GVR)
	if !ok {
		// NOTE(a-hilaly): this might mean that the GVR is not registered, or the workflow operator
		// is not found. We should probably handle this in a better way.
		return fmt.Errorf("no handler found for GVR: %s", gvrKey)
	}

	// this is worth a panic if it fails...
	handlerFunc, ok := genericHandler.(Handler)
	if !ok {
		return fmt.Errorf("invalid handler type for GVR: %s", gvrKey)
	}
	err := handlerFunc(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: oi.NamespacedKey}})
	if err != nil {
		handlerErrorsTotal.WithLabelValues(gvrKey).Inc()
	}
	return err
}

// gracefulShutdown performs a graceful shutdown of the controller.
func (dc *DynamicController) gracefulShutdown(timeout time.Duration) error {
	dc.log.Info("Starting graceful shutdown")

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var wg sync.WaitGroup
	dc.handlers.Range(func(key, value interface{}) bool {
		wg.Add(1)
		go func() {
			gvr := key.(schema.GroupVersionResource)
			dc.log.V(1).Info("Shutting down handler for GVR", "gvr", gvr)
			if err := dc.StopServiceGVK(ctx, gvr); err != nil {
				dc.log.Error(err, "Failed to stop service for GVR", "gvr", gvr)
			} else {
				dc.log.V(1).Info("Successfully stopped handler for GVR", "gvr", gvr)
			}
		}()
		return true
	})

	// Wait for all factories to shut down or timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		dc.log.Info("All factories shut down successfully")
	case <-ctx.Done():
		dc.log.Error(ctx.Err(), "Timeout waiting for factories to shut down")
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

// updateFunc is the update event handler for the GVR factories
func (dc *DynamicController) updateFunc(parentGVR schema.GroupVersionResource, old, new interface{}) {
	newObj, ok := new.(*metav1.PartialObjectMetadata)
	if !ok {
		dc.log.Error(nil, "failed to cast new object to unstructured")
		return
	}
	oldObj, ok := old.(*metav1.PartialObjectMetadata)
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

	dc.enqueueParent(parentGVR, new, "update")
}

// enqueueParent adds an object to the workqueue
func (dc *DynamicController) enqueueParent(parentGVR schema.GroupVersionResource, obj interface{}, eventType string) {
	namespacedKey, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		dc.log.Error(err, "Failed to get key for object", "eventType", eventType)
		return
	}

	objectIdentifiers := ObjectIdentifiers{
		NamespacedKey: namespacedKey,
		GVR:           parentGVR,
	}

	dc.log.V(1).Info("Enqueueing object",
		"objectIdentifiers", objectIdentifiers,
		"eventType", eventType)

	informerEventsTotal.WithLabelValues(parentGVR.String(), eventType).Inc()
	dc.queue.Add(objectIdentifiers)
}

// StartServingGVK registers a new GVK to the factories map safely.
func (dc *DynamicController) StartServingGVK(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	instanceHandler Handler,
	resourceGVRsToWatch []schema.GroupVersionResource,
) error {
	dc.log.V(1).Info("Registering new GVK", "gvr", gvr)

	if wrapper, exists := dc.factories.Load(gvr); exists {
		// Ensure resource handlers for a given GVK are consistent with the running wrapper.
		fw := wrapper.(*factoryWrapper)
		if err := dc.refreshResourceHandlers(gvr, resourceGVRsToWatch, fw); err != nil {
			return fmt.Errorf("failed to refresh resource handlers for instance gvr %s: %w", gvr, err)
		}
		fw.factory.Start(fw.ctx.Done())

		// Even thought the fw is already registered, we should still
		// update the instanceHandler, as it might have changed.
		dc.handlers.Store(gvr, instanceHandler)
		// trigger reconciliation of the corresponding resourceGVR's
		objs, err := dc.kubeClient.Resource(gvr).Namespace("").List(fw.ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("failed to list objects for GVR %s: %w", gvr, err)
		}
		for _, obj := range objs.Items {
			dc.enqueueParent(gvr, &obj, "update")
		}
		return nil
	}

	// Create a new factory
	factory := metadatainformer.NewFilteredSharedInformerFactory(
		dc.kubeClient,
		dc.config.ResyncPeriod,
		// Maybe we can make this configurable in the future. Thinking that
		// we might want to filter out some resources, by namespace or labels
		"", nil,
	)

	// TODO we can optimize this further by introducing a second factory that tweaks the list options
	// to filter out resources not relevant for kro on server side. This reduces the amount of watched objects
	// on the client side dramatically. For now we filter on client side so this should be fine.
	registrations, err := dc.registerResourceHandlers(gvr, resourceGVRsToWatch, factory)
	if err != nil {
		return fmt.Errorf("failed to register resource handlers for GVR %s: %w", gvr, err)
	}

	informer := factory.ForResource(gvr).Informer()
	// Set up event handlers

	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) { dc.enqueueParent(gvr, obj, "add") },
		UpdateFunc: func(oldObj, newObj interface{}) {
			dc.updateFunc(gvr, oldObj, newObj)
		},
		DeleteFunc: func(obj interface{}) { dc.enqueueParent(gvr, obj, "delete") },
	}); err != nil {
		dc.log.Error(err, "Failed to add event instanceHandler", "resourceGVR", gvr)
		return fmt.Errorf("failed to add event instanceHandler for GVR %s: %w", gvr, err)
	}
	if err := informer.SetWatchErrorHandler(func(r *cache.Reflector, err error) {
		dc.log.Error(err, "Watch error", "gvr", gvr)
	}); err != nil {
		dc.log.Error(err, "Failed to set watch error handler", "gvr", gvr)
	}

	dc.handlers.Store(gvr, instanceHandler)

	cancelableContext, cancel := context.WithCancel(ctx)
	dc.log.V(1).Info("Starting informer factory", "gvr", gvr)
	factory.Start(cancelableContext.Done())

	dc.log.V(1).Info("Waiting for cache sync", "gvr", gvr)
	startTime := time.Now()
	// Wait for cache sync with a timeout
	synced := factory.WaitForCacheSync(cancelableContext.Done())
	syncDuration := time.Since(startTime)
	informerSyncDuration.WithLabelValues(gvr.String()).Observe(syncDuration.Seconds())

	for gvr, synced := range synced {
		if !synced {
			cancel()
			return fmt.Errorf("failed to sync informer cache for GVR %s", gvr)
		}
	}

	dc.factories.Store(gvr, &factoryWrapper{
		factory:       factory,
		shutdown:      cancel,
		ctx:           cancelableContext,
		registrations: registrations,
	})
	gvrCount.Inc()
	dc.log.V(1).Info("Successfully registered GVK", "gvr", gvr)
	return nil
}

func (dc *DynamicController) registerResourceHandlers(parent schema.GroupVersionResource, resourceHandlers []schema.GroupVersionResource, factory metadatainformer.SharedInformerFactory) (map[schema.GroupVersionResource]cache.ResourceEventHandlerRegistration, error) {
	registrations := make(map[schema.GroupVersionResource]cache.ResourceEventHandlerRegistration, len(resourceHandlers))
	for _, resourceGVR := range resourceHandlers {
		informer := factory.ForResource(resourceGVR).Informer()
		registration, err := informer.AddEventHandler(dc.handlerForGVR(parent, resourceGVR))
		if err != nil {
			return nil, fmt.Errorf("failed to add event handler for GVR %s: %w", resourceGVR, err)
		}
		if err := informer.SetWatchErrorHandler(func(r *cache.Reflector, err error) {
			dc.log.Error(err, "Watch error on resource", "gvr", resourceGVR)
		}); err != nil {
			return nil, fmt.Errorf("failed to set watch error handler for GVR %s: %w", resourceGVR, err)
		}
		registrations[resourceGVR] = registration
	}
	return registrations, nil
}

func (dc *DynamicController) handlerForGVR(parent, gvr schema.GroupVersionResource) cache.ResourceEventHandler {
	handle := func(obj interface{}, eventType string) {
		objMeta, err := meta.Accessor(obj)
		if err != nil {
			dc.log.Error(err, "failed to get metadata accessor for object", "eventType", eventType,
				"resourceName", objMeta.GetName(),
				"resourceNamespace", objMeta.GetNamespace())
			return
		}

		// if there is no kro instance label set, we assume the object is irrelevant for the instance.
		labels := objMeta.GetLabels()
		owned, ok := labels[metadata.OwnedLabel]
		if !ok || owned != "true" {
			return
		}
		name, ok := labels[metadata.InstanceLabel]
		if !ok {
			return
		}
		namespace, ok := labels[metadata.InstanceNamespaceLabel]
		if !ok {
			return
		}

		parentGVK := metadata.GVRtoGVK(parent)
		pom := &metav1.PartialObjectMetadata{}
		pom.SetGroupVersionKind(parentGVK)
		pom.SetName(name)
		pom.SetNamespace(namespace)

		if err == nil {
			dc.log.Info("update from resource",
				"name", name,
				"namespace", namespace,
				"gvr", gvr,
				"resourceName", objMeta.GetName(),
				"resourceNamespace", objMeta.GetNamespace(),
				"eventType", eventType)
			dc.enqueueParent(parent, pom, eventType)
		}
	}
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { handle(obj, "add") },
		UpdateFunc: func(oldObj, newObj interface{}) { handle(newObj, "update") },
		DeleteFunc: func(obj interface{}) { handle(obj, "delete") },
	}
}

func (dc *DynamicController) refreshResourceHandlers(parent schema.GroupVersionResource, expectedGVRs []schema.GroupVersionResource, wrapper *factoryWrapper) error {
	for oldGVR, oldRegistration := range wrapper.registrations {
		if !slices.Contains(expectedGVRs, oldGVR) {
			informer := wrapper.factory.ForResource(oldGVR).Informer()
			if err := informer.RemoveEventHandler(oldRegistration); err != nil {
				return fmt.Errorf("failed to remove event handler for GVR %s: %w", oldGVR, err)
			}
			delete(wrapper.registrations, oldGVR) // Clean up registration map
		}
	}
	for _, resourceGVR := range expectedGVRs {
		if _, isRegistered := wrapper.registrations[resourceGVR]; isRegistered {
			continue
		}
		informer := wrapper.factory.ForResource(resourceGVR).Informer()
		registration, err := informer.AddEventHandler(dc.handlerForGVR(parent, resourceGVR))
		if err != nil {
			return fmt.Errorf("failed to add event handler for GVR %s: %w", resourceGVR, err)
		}
		wrapper.registrations[resourceGVR] = registration
	}
	return nil
}

// StopServiceGVK safely removes a GVK from the controller and cleans up associated resources.
func (dc *DynamicController) StopServiceGVK(_ context.Context, gvr schema.GroupVersionResource) error {
	dc.log.Info("Unregistering GVK", "gvr", gvr)

	// Retrieve the factory
	informerObj, ok := dc.factories.Load(gvr)
	if !ok {
		dc.log.V(1).Info("GVK not registered, nothing to unregister", "gvr", gvr)
		return nil
	}

	wrapper, ok := informerObj.(*factoryWrapper)
	if !ok {
		return fmt.Errorf("invalid factory type for GVR: %s", gvr)
	}

	// Stop the factory
	dc.log.V(1).Info("Stopping factory", "gvr", gvr)

	// Cancel the context to stop the factory
	wrapper.shutdown()
	// Wait for the factory to shut down
	wrapper.factory.Shutdown()

	// Remove the factory from the map
	dc.factories.Delete(gvr)

	// Unregister the handler if any
	dc.handlers.Delete(gvr)

	gvrCount.Dec()
	// Clean up any pending items in the queue for this GVR
	// NOTE(a-hilaly): This is a bit heavy.. maybe we can find a better way to do this.
	// Thinking that we might want to have a queue per GVR.
	// dc.cleanupQueue(gvr)
	// time.Sleep(1 * time.Second)
	// isStopped := wrapper.factory.ForResource(gvr).Informer().IsStopped()
	dc.log.V(1).Info("Successfully unregistered GVR", "gvr", gvr)
	return nil
}
