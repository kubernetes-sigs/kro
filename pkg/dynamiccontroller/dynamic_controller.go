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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/time/rate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	k8smetadata "k8s.io/client-go/metadata"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kubernetes-sigs/kro/pkg/requeue"
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
	// MinRetryDelay is the minimum delay before retrying an item in the queue
	MinRetryDelay time.Duration
	// MaxRetryDelay is the maximum delay before retrying an item in the queue
	MaxRetryDelay time.Duration
	// RateLimit is the maximum number of events processed per second
	RateLimit int
	// BurstLimit is the maximum number of events in a burst
	BurstLimit int
	// QueueShutdownTimeout is the maximum time to wait for the queue to drain before shutting down.
	QueueShutdownTimeout time.Duration
}

// Handler is used to actually perform the reconciliation logic for an instance GVR and will operate
// on a single instance of the resource received from the queue
type Handler func(ctx context.Context, req ctrl.Request) error

// ObjectIdentifiers holds the key and GVR of the object to reconcile.
type ObjectIdentifiers struct {
	types.NamespacedName
	GVR schema.GroupVersionResource
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
//
// The DynamicController uses a layered architecture:
//
//  1. WatchManager: manages informer lifecycle per GVR (reference-counted).
//
//  2. WatchCoordinator: aggregates watch requests from all instance reconcilers,
//     maintains reverse indexes for event routing, and manages shared watches
//     on the WatchManager.
//
//  3. DynamicController: orchestrates parent watches (one per RGD), the work queue,
//     and handler dispatch. Child/external resource watches are handled by the
//     coordinator based on requests from instance reconcilers.
type DynamicController struct {
	// Parent run context, inherited by all informer stop contexts.
	// It is set by Start and meant to be used to register the controller context with a global handler
	// such as the controller-runtime manager. Thread-safe.
	ctx atomic.Pointer[context.Context]

	// mu protects parentWatches.
	mu sync.Mutex

	config Config
	log    logr.Logger

	// Shared watch lifecycle management.
	watches *WatchManager

	// Instance-level watch coordination.
	coordinator *WatchCoordinator

	// Parent watch tracking: maps parent GVR to its event handler registration.
	parentWatches map[schema.GroupVersionResource]cache.ResourceEventHandlerRegistration

	// Handler dispatch.
	handlers sync.Map // map[schema.GroupVersionResource]Handler (thread-safe on its own)
	queue    workqueue.TypedRateLimitingInterface[ObjectIdentifiers]
	mapper   meta.RESTMapper
}

// NewDynamicController creates a new DynamicController.
func NewDynamicController(
	log logr.Logger,
	config Config,
	kubeClient k8smetadata.Interface,
	mapper meta.RESTMapper,
) *DynamicController {
	logger := log.WithName("dynamic-controller")

	dc := &DynamicController{
		config:        config,
		log:           logger,
		parentWatches: make(map[schema.GroupVersionResource]cache.ResourceEventHandlerRegistration),
		mapper:        mapper,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.NewTypedMaxOfRateLimiter(
			workqueue.NewTypedItemExponentialFailureRateLimiter[ObjectIdentifiers](config.MinRetryDelay, config.MaxRetryDelay),
			&workqueue.TypedBucketRateLimiter[ObjectIdentifiers]{Limiter: rate.NewLimiter(rate.Limit(config.RateLimit), config.BurstLimit)},
		), workqueue.TypedRateLimitingQueueConfig[ObjectIdentifiers]{Name: "dynamic-controller-queue"}),
	}

	// WatchManager routes all informer events through the coordinator.
	dc.watches = NewWatchManager(kubeClient, config.ResyncPeriod, dc.routeChildEvent, logger)

	return dc
}

// Start starts workers and blocks until ctx.Done().
func (dc *DynamicController) Start(ctx context.Context) error {
	if !dc.ctx.CompareAndSwap(nil, &ctx) {
		return fmt.Errorf("already running")
	}

	// Initialize the coordinator now that we have a context.
	dc.coordinator = NewWatchCoordinator(dc.watches, dc.enqueueInstance, dc.log)

	defer utilruntime.HandleCrash()

	dc.log.Info("Starting dynamic controller")
	defer dc.log.Info("Shutting down dynamic controller")

	// Workers.
	for i := 0; i < dc.config.Workers; i++ {
		go wait.UntilWithContext(ctx, dc.worker, time.Second)
	}

	<-ctx.Done()
	return dc.gracefulShutdown()
}

// Coordinator returns the WatchCoordinator for use by instance controllers.
func (dc *DynamicController) Coordinator() *WatchCoordinator {
	return dc.coordinator
}

// routeChildEvent is the EventHandler callback given to WatchManager.
// It delegates to the coordinator for child/external resource event routing.
func (dc *DynamicController) routeChildEvent(event Event) {
	if dc.coordinator != nil {
		dc.coordinator.RouteEvent(event)
	}
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
	if err == nil {
		dc.queue.Forget(item)
		return true
	}

	gvrKey := keyFromGVR(item.GVR)

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
		// we only check here for this not found error here because we want explicit requeue signals to have priority
		if apierrors.IsNotFound(err) {
			dc.log.V(1).Info("item no longer exists, dropping from queue", "item", item)
			dc.queue.Forget(item)
			return true
		}
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
	gvrKey := keyFromGVR(oi.GVR)
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

// enqueueParent enqueues an instance directly from a parent watch event.
func (dc *DynamicController) enqueueParent(parentGVR schema.GroupVersionResource, event Event) {
	oi := ObjectIdentifiers{
		NamespacedName: types.NamespacedName{
			Namespace: event.Namespace,
			Name:      event.Name,
		},
		GVR: parentGVR,
	}
	dc.log.V(1).Info("Enqueueing object", "objectIdentifiers", oi, "eventType", event.Type)
	informerEventsTotal.WithLabelValues(parentGVR.String(), string(event.Type)).Inc()
	dc.queue.Add(oi)
}

// enqueueInstance is the EnqueueFunc used by the coordinator to route
// child/external resource events to the owning instance.
func (dc *DynamicController) enqueueInstance(parentGVR schema.GroupVersionResource, instance types.NamespacedName) {
	oi := ObjectIdentifiers{
		NamespacedName: instance,
		GVR:            parentGVR,
	}
	dc.log.V(2).Info("Coordinator routed event to instance", "objectIdentifiers", oi)
	dc.queue.Add(oi)
}

// Register registers a parent GVR with a handler. The coordinator discovers
// child/external GVRs dynamically from Watch() calls made by instance reconcilers.
func (dc *DynamicController) Register(
	_ context.Context,
	parent schema.GroupVersionResource,
	instanceHandler Handler,
) error {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	ctx := dc.ctx.Load()
	if ctx == nil {
		return fmt.Errorf("dynamic controller not started")
	}

	// Store handler.
	dc.handlers.Store(parent, instanceHandler)

	// Create parent watch if it doesn't exist.
	if _, exists := dc.parentWatches[parent]; !exists {
		// Ensure informer is running (non-blocking).
		dc.watches.EnsureWatch(parent)

		inf := dc.watches.GetInformer(parent)
		if inf == nil {
			dc.handlers.Delete(parent)
			return fmt.Errorf("add parent handler %s: informer not found after EnsureWatch", parent)
		}

		// Wait for the parent informer to sync so we can enumerate existing instances.
		if !cache.WaitForCacheSync((*ctx).Done(), inf.HasSynced) {
			dc.handlers.Delete(parent)
			return fmt.Errorf("add parent handler %s: cache sync failed", parent)
		}

		// Register event handler directly on the parent informer.
		parentHandler := cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				dc.enqueueFromInformer(parent, obj, EventAdd)
			},
			UpdateFunc: func(_, newObj interface{}) {
				dc.enqueueFromInformer(parent, newObj, EventUpdate)
			},
			DeleteFunc: func(obj interface{}) {
				dc.enqueueFromInformer(parent, obj, EventDelete)
			},
		}
		reg, err := inf.AddEventHandler(parentHandler)
		if err != nil {
			dc.handlers.Delete(parent)
			return fmt.Errorf("add parent handler %s: %w", parent, err)
		}
		dc.parentWatches[parent] = reg

		gvrCount.Inc()
		handlerAttachTotal.WithLabelValues("parent").Inc()
		handlerCount.WithLabelValues("parent").Inc()
		dc.log.V(1).Info("Attached parent watch", "gvr", parent)
	}

	// Enqueue existing instances from parent cache.
	if inf := dc.watches.GetInformer(parent); inf != nil && !inf.IsStopped() {
		objects := inf.GetStore().List()
		for _, obj := range objects {
			mobj, err := meta.Accessor(obj)
			if err != nil {
				continue
			}
			dc.enqueueParent(parent, Event{
				Type:      EventUpdate,
				GVR:       parent,
				Name:      mobj.GetName(),
				Namespace: mobj.GetNamespace(),
			})
		}
	}

	dc.log.V(1).Info("Successfully registered GVR", "gvr", keyFromGVR(parent))
	return nil
}

// enqueueFromInformer converts a raw informer callback into an enqueue call.
func (dc *DynamicController) enqueueFromInformer(parentGVR schema.GroupVersionResource, obj interface{}, eventType EventType) {
	mobj, err := meta.Accessor(obj)
	if err != nil {
		dc.log.Error(err, "Failed to get meta for parent object")
		return
	}
	dc.enqueueParent(parentGVR, Event{
		Type:      eventType,
		GVR:       parentGVR,
		Name:      mobj.GetName(),
		Namespace: mobj.GetNamespace(),
	})
}

// Deregister removes a parent GVR handler and cleans up coordinator state.
func (dc *DynamicController) Deregister(_ context.Context, parent schema.GroupVersionResource) error {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	gvrKey := keyFromGVR(parent)

	// Clean up coordinator state for all instances of this parent.
	if dc.coordinator != nil {
		dc.coordinator.RemoveParentGVR(parent)
	}

	// Remove parent event handler registration from the informer.
	if reg, exists := dc.parentWatches[parent]; exists {
		if inf := dc.watches.GetInformer(parent); inf != nil {
			if err := inf.RemoveEventHandler(reg); err != nil {
				dc.log.Error(err, "failed to remove parent event handler", "parent", gvrKey)
			}
		}
		delete(dc.parentWatches, parent)

		// Stop the parent informer — it's no longer needed.
		dc.watches.StopWatch(parent)

		gvrCount.Dec()
		handlerDetachTotal.WithLabelValues("parent").Inc()
		handlerCount.WithLabelValues("parent").Dec()
		dc.log.V(1).Info("Detached parent watch", "gvr", parent)
	}

	dc.handlers.Delete(parent)

	dc.log.V(1).Info("Successfully unregistered GVR", "gvr", gvrKey)
	return nil
}

func (dc *DynamicController) gracefulShutdown() error {
	dc.log.Info("Starting graceful shutdown")

	dc.watches.Shutdown()

	queueShutdownDone := make(chan struct{})
	go func() {
		dc.queue.ShutDown()
		close(queueShutdownDone)
	}()

	ctx := context.Background()
	var cancel context.CancelFunc
	if dc.config.QueueShutdownTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, dc.config.QueueShutdownTimeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	select {
	case <-queueShutdownDone:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("timeout waiting for queue to shutdown: %w", ctx.Err())
	}
}

// keyFromGVR returns a compact, allocation-efficient string key for the given
// GroupVersionResource in the canonical "group/version/resource" format.
// Unlike [schema.GroupVersionResource.String], it omits labels and avoids extra
// allocations, making it suitable for use as map keys, metrics labels, and cache identifiers.
func keyFromGVR(gvr schema.GroupVersionResource) string {
	var b strings.Builder
	if gvr.Group != "" {
		b.WriteString(gvr.Group)
	}
	if gvr.Version != "" {
		b.WriteRune('/')
		b.WriteString(gvr.Version)
	}
	if gvr.Resource != "" {
		b.WriteRune('/')
		b.WriteString(gvr.Resource)
	}
	return b.String()
}
