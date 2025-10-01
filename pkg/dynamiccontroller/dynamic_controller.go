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
package dynamiccontroller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/time/rate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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

// Config holds the configuration for DynamicController.
type Config struct {
	Workers         int
	ResyncPeriod    time.Duration
	QueueMaxRetries int
	ShutdownTimeout time.Duration
	MinRetryDelay   time.Duration
	MaxRetryDelay   time.Duration
	RateLimit       int
	BurstLimit      int
}

type Handler func(ctx context.Context, req ctrl.Request) error

// ObjectIdentifiers holds the key and GVR of the object to reconcile.
type ObjectIdentifiers struct {
	NamespacedKey string
	GVR           schema.GroupVersionResource
}

// perGVRWatch keeps per-GVR informer state under one shared factory.
// It has its own mutex to allow safe concurrent updates of handler bookkeeping
// (handlers map) without blocking unrelated GVR operations.
type perGVRWatch struct {
	informer cache.SharedIndexInformer

	// Map of handlerID → registration handle returned by AddEventHandler.
	handlers map[string]cache.ResourceEventHandlerRegistration

	// Context for this informer only. Cancelled when refCount=0 or controller shutdown.
	ctx    context.Context
	cancel context.CancelFunc

	// Protects handlers. Required because multiple goroutines may add/remove handlers concurrently
	// This lock does not protect dc.watches (that is guarded by dc.mu).
	mu sync.Mutex
}

// registration tracks one parent GVR registration and its child handler IDs.
// Each parent may own one "parent handler" and multiple "child handlers".
type registration struct {
	parentGVR schema.GroupVersionResource

	parentHandlerID string
	childHandlerIDs map[schema.GroupVersionResource]string
}

// DynamicController manages all handlers and informers.
// It uses two levels of locking:
//
//  1. dc.mu protects *global maps* (watches and registrations).
//     This ensures consistency when adding/removing GVRs or
//     attaching/detaching children.
//
//  2. Each perGVRWatch has its own w.mu to protect *per-informer state*
//     (handler map). This allows concurrent updates to
//     different GVRs without blocking each other.
type DynamicController struct {
	config Config
	log    logr.Logger

	// Shared factory for all informers managed by this controller.
	factory metadatainformer.SharedInformerFactory
	client  k8smetadata.Interface

	// Map of active informers per GVR.
	// Guarded by mu.
	watches map[schema.GroupVersionResource]*perGVRWatch
	// Map of parent registrations per GVR.
	// Guarded by mu.
	registrations map[schema.GroupVersionResource]*registration
	// Global mutex protecting watches and registrations.
	// Required because StartServingGVK and StopServiceGVK may run concurrently,
	// and because Run or gracefulShutdown may also traverse these maps.
	mu sync.Mutex

	// Latest handler for each parent GVR used by syncFunc.
	handlers sync.Map // map[schema.GroupVersionResource]Handler (thread-safe on its own)
	queue    workqueue.TypedRateLimitingInterface[ObjectIdentifiers]

	// Parent run context, inherited by all informer stop contexts.
	// set by Run
	ctx context.Context
}

// NewDynamicController creates a new DynamicController.
func NewDynamicController(
	log logr.Logger,
	config Config,
	kubeClient k8smetadata.Interface,
) *DynamicController {
	logger := log.WithName("dynamic-controller")

	// Single shared factory.
	factory := metadatainformer.NewFilteredSharedInformerFactory(
		kubeClient,
		config.ResyncPeriod,
		"",
		nil,
	)

	return &DynamicController{
		config:        config,
		log:           logger,
		factory:       factory,
		client:        kubeClient,
		watches:       make(map[schema.GroupVersionResource]*perGVRWatch),
		registrations: make(map[schema.GroupVersionResource]*registration),
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.NewTypedMaxOfRateLimiter(
			workqueue.NewTypedItemExponentialFailureRateLimiter[ObjectIdentifiers](config.MinRetryDelay, config.MaxRetryDelay),
			&workqueue.TypedBucketRateLimiter[ObjectIdentifiers]{Limiter: rate.NewLimiter(rate.Limit(config.RateLimit), config.BurstLimit)},
		), workqueue.TypedRateLimitingQueueConfig[ObjectIdentifiers]{Name: "dynamic-controller-queue"}),
	}
}

// Start starts workers and blocks until ctx.Done().
func (dc *DynamicController) Start(ctx context.Context) error {
	if dc.ctx != nil {
		return fmt.Errorf("already running")
	}

	defer utilruntime.HandleCrash()

	dc.log.Info("Starting dynamic controller")
	defer dc.log.Info("Shutting down dynamic controller")

	dc.ctx = ctx

	// Workers.
	for i := 0; i < dc.config.Workers; i++ {
		go wait.UntilWithContext(ctx, dc.worker, time.Second)
	}

	<-ctx.Done()
	return dc.gracefulShutdown(dc.config.ShutdownTimeout)
}

func (dc *DynamicController) worker(ctx context.Context) {
	for dc.processNextWorkItem(ctx) {
	}
}

func (dc *DynamicController) processNextWorkItem(ctx context.Context) bool {
	item, shutdown := dc.queue.Get()
	if shutdown {
		return false
	}
	defer dc.queue.Done(item)

	// metric: queueLength
	queueLength.Set(float64(dc.queue.Len()))

	err := dc.syncFunc(ctx, item)
	if err == nil || apierrors.IsNotFound(err) {
		dc.queue.Forget(item)
		return true
	}

	gvrKey := item.GVR.String()

	switch typedErr := err.(type) {
	case *requeue.NoRequeue:
		dc.log.Error(typedErr, "Error syncing item, not requeuing", "item", item)
		requeueTotal.WithLabelValues(gvrKey, "no_requeue").Inc()
		dc.queue.Forget(item)
	case *requeue.RequeueNeeded:
		dc.log.V(1).Info("Requeue needed", "item", item, "error", typedErr)
		requeueTotal.WithLabelValues(gvrKey, "requeue").Inc()
		dc.queue.Add(item)
	case *requeue.RequeueNeededAfter:
		dc.log.V(1).Info("Requeue needed after delay", "item", item, "error", typedErr, "delay", typedErr.Duration())
		requeueTotal.WithLabelValues(gvrKey, "requeue_after").Inc()
		dc.queue.AddAfter(item, typedErr.Duration())
	default:
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

func (dc *DynamicController) syncFunc(ctx context.Context, oi ObjectIdentifiers) error {
	gvrKey := oi.GVR.String()
	dc.log.V(1).Info("Syncing resourcegraphdefinition instance request", "gvr", gvrKey, "namespacedKey", oi.NamespacedKey)

	startTime := time.Now()
	defer func() {
		duration := time.Since(startTime)
		reconcileDuration.WithLabelValues(gvrKey).Observe(duration.Seconds())
		reconcileTotal.WithLabelValues(gvrKey).Inc()
		dc.log.V(1).Info("Finished syncing resourcegraphdefinition instance request",
			"gvr", gvrKey, "namespacedKey", oi.NamespacedKey, "duration", duration)
	}()

	genericHandler, ok := dc.handlers.Load(oi.GVR)
	if !ok {
		return fmt.Errorf("no handler found for GVR: %s", gvrKey)
	}
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

func (dc *DynamicController) enqueueParent(parentGVR schema.GroupVersionResource, obj interface{}, eventType string) {
	namespacedKey, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		dc.log.Error(err, "Failed to get key for object", "eventType", eventType)
		return
	}

	oi := ObjectIdentifiers{NamespacedKey: namespacedKey, GVR: parentGVR}
	dc.log.V(1).Info("Enqueueing object", "objectIdentifiers", oi, "eventType", eventType)

	informerEventsTotal.WithLabelValues(parentGVR.String(), eventType).Inc()
	dc.queue.Add(oi)
}

func (dc *DynamicController) updateFunc(parentGVR schema.GroupVersionResource, oldObj, newObj interface{}) {
	newMeta, ok := newObj.(*metav1.PartialObjectMetadata)
	if !ok {
		dc.log.Error(nil, "failed to cast new object to PartialObjectMetadata")
		return
	}
	oldMeta, ok := oldObj.(*metav1.PartialObjectMetadata)
	if !ok {
		dc.log.Error(nil, "failed to cast old object to PartialObjectMetadata")
		return
	}
	if newMeta.GetGeneration() == oldMeta.GetGeneration() {
		dc.log.V(2).Info("Skipping update due to unchanged generation",
			"name", newMeta.GetName(), "namespace", newMeta.GetNamespace(), "generation", newMeta.GetGeneration())
		return
	}
	dc.enqueueParent(parentGVR, newObj, "update")
}

// StartServingGVK registers parent and children via reconciliation.
func (dc *DynamicController) StartServingGVK(
	_ context.Context,
	parent schema.GroupVersionResource,
	instanceHandler Handler,
	resourceGVRsToWatch []schema.GroupVersionResource,
) error {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	reg, exists := dc.registrations[parent]
	if !exists {
		reg = &registration{
			parentGVR:       parent,
			childHandlerIDs: make(map[schema.GroupVersionResource]string),
		}
		dc.registrations[parent] = reg
	}

	if err := dc.reconcileParentLocked(parent, instanceHandler, reg); err != nil {
		return err
	}
	if err := dc.reconcileChildrenLocked(parent, resourceGVRsToWatch, reg); err != nil {
		return err
	}

	// kick reconciliation for existing parent objects
	objs, err := dc.factory.ForResource(parent).Lister().List(labels.NewSelector())
	if err != nil {
		return fmt.Errorf("list parent objects for %s: %w", parent, err)
	}
	for _, obj := range objs {
		dc.enqueueParent(parent, obj, "update")
	}

	dc.log.V(1).Info("Successfully registered GVR", "gvr", parent)
	return nil
}

// StopServiceGVK clears parent and children.
func (dc *DynamicController) StopServiceGVK(_ context.Context, parent schema.GroupVersionResource) error {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	reg, exists := dc.registrations[parent]
	if !exists {
		return nil
	}

	if err := dc.reconcileChildrenLocked(parent, nil, reg); err != nil {
		dc.log.Error(err, "failed to detach children", "parent", parent)
	}
	if err := dc.reconcileParentLocked(parent, nil, reg); err != nil {
		dc.log.Error(err, "failed to detach parent", "parent", parent)
	}

	delete(dc.registrations, parent)
	dc.log.V(1).Info("Successfully unregistered GVR", "gvr", parent)
	return nil
}

// ----- internal helpers -----

// ensureWatchLocked creates and starts the per-GVR informer with its own stop channel
// under the shared factory. Must be called with dc.mu held.
func (dc *DynamicController) ensureWatchLocked(gvr schema.GroupVersionResource) (*perGVRWatch, error) {
	if w, ok := dc.watches[gvr]; ok {
		return w, nil
	}

	inf := dc.factory.ForResource(gvr).Informer()

	if inf.IsStopped() {
		// Set watch error handler before starting the informer.
		if err := inf.SetWatchErrorHandler(func(r *cache.Reflector, err error) {
			dc.log.Error(err, "Watch error", "gvr", gvr)
		}); err != nil {
			dc.log.Error(err, "Failed to set watch error handler", "gvr", gvr)
		}
	}

	ctx, cancel := context.WithCancel(dc.ctx)

	w := &perGVRWatch{
		informer: inf,
		handlers: make(map[string]cache.ResourceEventHandlerRegistration),
		ctx:      ctx,
		cancel:   cancel,
	}
	dc.watches[gvr] = w

	// Start only newly created (unstarted) informers with this stop channel.
	// Because we hold dc.mu, no other GVR creation interleaves.
	dc.factory.Start(ctx.Done())

	// Wait for this informer's cache to sync without blocking on others.
	start := time.Now()
	if ok := cache.WaitForCacheSync(ctx.Done(), inf.HasSynced); !ok {
		cancel()
		delete(dc.watches, gvr)
		return nil, fmt.Errorf("failed to sync informer for %s", gvr)
	}
	informerSyncDuration.WithLabelValues(gvr.String()).Observe(time.Since(start).Seconds())

	return w, nil
}

// reconcileParentLocked ensures a parent watch exists and has exactly one handler.
// If instanceHandler is nil, the parent handler is removed.
// Must be called with dc.mu held.
func (dc *DynamicController) reconcileParentLocked(
	parent schema.GroupVersionResource,
	instanceHandler Handler,
	reg *registration,
) error {
	if instanceHandler == nil {
		// remove parent handler if present
		if reg.parentHandlerID != "" {
			if w, ok := dc.watches[parent]; ok {
				stopped, err := w.removeHandler(reg.parentHandlerID)
				if err != nil {
					return fmt.Errorf("remove parent handler %s: %w", parent, err)
				}
				if stopped {
					delete(dc.watches, parent)
				}
			}
			reg.parentHandlerID = ""
			dc.handlers.Delete(parent)
			gvrCount.Dec()
			dc.log.V(1).Info("Detached parent", "gvr", parent)
		}
		return nil
	}

	// ensure watch
	w, err := dc.ensureWatchLocked(parent)
	if err != nil {
		return fmt.Errorf("ensure parent watch %s: %w", parent, err)
	}

	// create handler if missing
	if reg.parentHandlerID == "" {
		handlerID := "parent:" + parent.String()
		if err := w.addHandler(handlerID, cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { dc.enqueueParent(parent, obj, "add") },
			UpdateFunc: func(oldObj, newObj interface{}) { dc.updateFunc(parent, oldObj, newObj) },
			DeleteFunc: func(obj interface{}) { dc.enqueueParent(parent, obj, "delete") },
		}); err != nil {
			return fmt.Errorf("add parent handler %s: %w", parent, err)
		}
		reg.parentHandlerID = handlerID
		gvrCount.Inc()
		dc.log.V(1).Info("Attached parent", "gvr", parent)
	}

	// always update latest handler
	dc.handlers.Store(parent, instanceHandler)

	return nil
}

// reconcileChildrenLocked ensures that reg.childHandlerIDs matches the desired set.
// Must be called with dc.mu held.
func (dc *DynamicController) reconcileChildrenLocked(
	parent schema.GroupVersionResource,
	desired []schema.GroupVersionResource,
	reg *registration,
) error {
	desiredSet := make(map[schema.GroupVersionResource]struct{}, len(desired))
	for _, g := range desired {
		desiredSet[g] = struct{}{}
	}

	// remove obsolete
	for child, id := range reg.childHandlerIDs {
		if _, keep := desiredSet[child]; !keep {
			if w, ok := dc.watches[child]; ok {
				stopped, err := w.removeHandler(id)
				if err != nil {
					return fmt.Errorf("remove child handler %s: %w", child, err)
				}
				if stopped {
					delete(dc.watches, child)
				}
			}
			delete(reg.childHandlerIDs, child)
			dc.log.V(1).Info("Detached child", "parent", parent, "gvr", child)
		}
	}

	// add missing
	for child := range desiredSet {
		if _, exists := reg.childHandlerIDs[child]; exists {
			continue
		}
		w, err := dc.ensureWatchLocked(child)
		if err != nil {
			return fmt.Errorf("ensure child watch %s: %w", child, err)
		}
		handlerID := "child:" + parent.String() + "->" + child.String()
		if err := w.addHandler(handlerID, dc.handlerForChildGVR(parent, child)); err != nil {
			return fmt.Errorf("add child handler %s: %w", child, err)
		}
		reg.childHandlerIDs[child] = handlerID
		dc.log.V(1).Info("Attached child", "parent", parent, "gvr", child)
	}

	return nil
}

// gracefulShutdown cancels all watches and shuts down the queue.
// Since cancel is synchronous and ShutDown blocks until the queue drains,
// no extra goroutines or waitgroups are needed.
func (dc *DynamicController) gracefulShutdown(timeout time.Duration) error {
	dc.log.Info("Starting graceful shutdown")

	// cancel all per-GVR contexts
	dc.mu.Lock()
	for gvr, w := range dc.watches {
		dc.log.V(1).Info("Cancelling watch", "gvr", gvr)
		w.cancel()
	}
	dc.mu.Unlock()

	// stop the queue (blocks until drained)
	done := make(chan struct{})
	go func() {
		dc.queue.ShutDown()
		close(done)
	}()

	select {
	case <-done:
		dc.log.Info("Queue shut down, all watches cancelled")
		return nil
	case <-time.After(timeout):
		err := fmt.Errorf("timeout after %s during shutdown", timeout)
		dc.log.Error(err, "Graceful shutdown timed out")
		return err
	}
}

func (w *perGVRWatch) addHandler(id string, h cache.ResourceEventHandler) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	reg, err := w.informer.AddEventHandler(h)
	if err != nil {
		return err
	}
	w.handlers[id] = reg
	return nil
}

func (w *perGVRWatch) removeHandler(id string) (stopped bool, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	reg, ok := w.handlers[id]
	if !ok {
		return false, nil
	}
	delete(w.handlers, id)

	if err := w.informer.RemoveEventHandler(reg); err != nil {
		return false, err
	}
	if len(w.handlers) == 0 {
		w.cancel()
		return true, nil
	}
	return false, nil
}

func (dc *DynamicController) handlerForChildGVR(parent, child schema.GroupVersionResource) cache.ResourceEventHandler {
	handle := func(obj interface{}, eventType string) {
		objMeta, err := meta.Accessor(obj)
		if err != nil {
			dc.log.Error(err, "failed to get metadata accessor for object", "eventType", eventType)
			return
		}
		lbls := objMeta.GetLabels()
		owned, ok := lbls[metadata.OwnedLabel]
		if !ok || owned != "true" {
			return
		}
		name, ok := lbls[metadata.InstanceLabel]
		if !ok {
			return
		}
		namespace, ok := lbls[metadata.InstanceNamespaceLabel]
		if !ok {
			return
		}

		parentGVK := metadata.GVRtoGVK(parent)
		pom := &metav1.PartialObjectMetadata{}
		pom.SetGroupVersionKind(parentGVK)
		pom.SetName(name)
		pom.SetNamespace(namespace)

		dc.log.V(1).Info("Child triggered parent reconciliation",
			"parent", parent.String(),
			"child", child.String(),
			"eventType", eventType,
			"childName", objMeta.GetName(),
			"childNamespace", objMeta.GetNamespace(),
			"targetName", name,
			"targetNamespace", namespace,
		)
		dc.enqueueParent(parent, pom, eventType)
	}
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { handle(obj, "add") },
		UpdateFunc: func(oldObj, newObj interface{}) { handle(newObj, "update") },
		DeleteFunc: func(obj interface{}) { handle(obj, "delete") },
	}
}
