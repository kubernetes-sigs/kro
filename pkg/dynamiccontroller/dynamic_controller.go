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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	k8smetadata "k8s.io/client-go/metadata"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kubernetes-sigs/kro/pkg/dynamiccontroller/internal"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
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
	types.NamespacedName
	GVR schema.GroupVersionResource
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

	client k8smetadata.Interface

	// Map of active informers per GVR.
	// Guarded by mu.
	watches map[schema.GroupVersionResource]*internal.LazyInformer
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

	return &DynamicController{
		config:        config,
		log:           logger,
		client:        kubeClient,
		watches:       make(map[schema.GroupVersionResource]*internal.LazyInformer),
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
	return dc.gracefulShutdown()
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

	handler, ok := dc.handlers.Load(item.GVR)
	if !ok {
		// this can happen if the handler was removed and we still have items in flight in the queue.
		dc.log.V(1).Info("handler for gvr no longer exists, dropping item", "item", item)
		dc.queue.Forget(item)
		return true
	}

	err := dc.syncFunc(ctx, item, handler.(Handler))
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

func (dc *DynamicController) syncFunc(ctx context.Context, oi ObjectIdentifiers, handler Handler) error {
	gvrKey := oi.GVR.String()
	dc.log.V(1).Info("Syncing object", "gvr", gvrKey, "key", oi.NamespacedName)

	startTime := time.Now()
	defer func() {
		duration := time.Since(startTime)
		reconcileDuration.WithLabelValues(gvrKey).Observe(duration.Seconds())
		reconcileTotal.WithLabelValues(gvrKey).Inc()
		dc.log.V(1).Info("Finished syncing object",
			"gvr", gvrKey, "key", oi.NamespacedName, "duration", duration)
	}()

	err := handler(ctx, ctrl.Request{NamespacedName: oi.NamespacedName})
	if err != nil {
		handlerErrorsTotal.WithLabelValues(gvrKey).Inc()
	}
	return err
}

func (dc *DynamicController) enqueueParent(parentGVR schema.GroupVersionResource, obj interface{}, eventType string) {
	mobj, err := meta.Accessor(obj)
	if err != nil {
		dc.log.Error(err, "Failed to get meta for object to enqueue", "eventType", eventType)
		return
	}

	oi := ObjectIdentifiers{NamespacedName: types.NamespacedName{
		Namespace: mobj.GetNamespace(),
		Name:      mobj.GetName(),
	}, GVR: parentGVR}
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

// Register registers parent and children via reconciliation.
func (dc *DynamicController) Register(
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
	if w, ok := dc.watches[parent]; ok && !w.Informer().IsStopped() {
		// Use informer cache if running to repopulate the queue.
		for _, obj := range w.Informer().GetStore().List() {
			dc.enqueueParent(parent, obj, "update")
		}
	}

	dc.log.V(1).Info("Successfully registered GVR", "gvr", parent)
	return nil
}

// Deregister clears parent and children and drops any queued items for the parent GVR.
func (dc *DynamicController) Deregister(_ context.Context, parent schema.GroupVersionResource) error {
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

func (dc *DynamicController) ensureWatchLocked(
	gvr schema.GroupVersionResource,
) *internal.LazyInformer {
	if w, ok := dc.watches[gvr]; ok {
		return w
	}

	// Create per-GVR watch wrapper (informer created lazily on first handler)
	w := internal.NewLazyInformer(dc.client, gvr, dc.config.ResyncPeriod, nil, dc.log)
	dc.watches[gvr] = w
	return w
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
				stopped, err := w.RemoveHandler(reg.parentHandlerID)
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
	w := dc.ensureWatchLocked(parent)

	// create handler if missing
	if reg.parentHandlerID == "" {
		handlerID := "parent:" + parent.String()
		if err := w.AddHandler(dc.ctx, handlerID, cache.ResourceEventHandlerFuncs{
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
				stopped, err := w.RemoveHandler(id)
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
		w := dc.ensureWatchLocked(child)
		handlerID := "child:" + parent.String() + "->" + child.String()
		if err := w.AddHandler(dc.ctx, handlerID, dc.handlerForChildGVR(parent, child)); err != nil {
			return fmt.Errorf("add child handler %s: %w", child, err)
		}
		reg.childHandlerIDs[child] = handlerID
		dc.log.V(1).Info("Attached child", "parent", parent, "gvr", child)
	}

	return nil
}

func (dc *DynamicController) gracefulShutdown() error {
	dc.log.Info("Starting graceful shutdown")

	dc.mu.Lock()
	for gvr, w := range dc.watches {
		dc.log.V(1).Info("Stopping watch", "gvr", gvr)
		w.Shutdown()
	}
	dc.mu.Unlock()

	done := make(chan struct{})
	go func() {
		dc.queue.ShutDown()
		close(done)
	}()

	select {
	case <-done:
		dc.log.Info("Queue shut down, all watches stopped")
		return nil
	}
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
