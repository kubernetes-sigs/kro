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

package dynamiccontroller

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/gobuffalo/flect"
	"golang.org/x/time/rate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/dynamic/dynamiclister"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/aws/symphony/api/v1alpha1"
	"github.com/aws/symphony/internal/graphexec"
	"github.com/aws/symphony/internal/k8smetadata"
	"github.com/aws/symphony/internal/kubernetes"
	"github.com/aws/symphony/internal/requeue"
	"github.com/aws/symphony/internal/resourcegroup"
	"github.com/aws/symphony/internal/typesystem/celextractor"
)

// DynamicController is a controller that can be used to create and manage "micro" controllers
// that are used to manage ResourceGroup instances in a cluster.
type DynamicController struct {
	// name is an identifier for this particular controller instance. Useless but I like naming things.
	name string
	// kubeClient is a dynamic client to the Kubernetes cluster.
	kubeClient *dynamic.DynamicClient
	// synced is a function that returns true when all the informers have synced.
	synced cache.InformerSynced
	// workflowOperators is a map of the registered workflow operators
	//
	// Thoughts: I originally thought that each ResourceGroup would be "compiled" into a
	// a set of workflow steps that are specific to the ResourceGroup. But now I think
	// there is a way to generalize the workflow steps. It should be possible to create
	// a generic set of workflow steps that can be applied to any ResourceGroup.
	workflowOperators map[schema.GroupVersionResource]*graphexec.Controller
	// handler is the function that will be called when a new work item is added
	// to the queue. The argument to the handler is an interface that should be
	// castable to the appropriate type.
	//
	// NOTE(a-hilaly) maybe unstructured.Unstructured is a better choice here.
	handler func(context.Context, ctrl.Request) error
	// queue is where incoming work is placed to de-dup and to allow "easy"
	// rate limited requeues.
	//
	// Might need multiple queues? one for each registered resource group? How about priority queues?
	queue workqueue.RateLimitingInterface
	// informers is a the map of the registered informers
	informers map[schema.GroupVersionResource]dynamicinformer.DynamicSharedInformerFactory
	// cancelFuncs is a map of the cancel functions for the informers
	cancelFuncs map[schema.GroupVersionResource]context.CancelFunc
	// Protects access to the informers map. Could have been a sync.Map but we need to
	// optimize for reads.
	mu sync.RWMutex
	// listers is a map of the registered listers. Not sure if we need this.
	listers map[schema.GroupVersionResource]dynamiclister.Lister

	hasSyncedFunctions map[schema.GroupVersionResource]func() bool

	log     logr.Logger
	rootLog logr.Logger

	symphonyLabeler k8smetadata.Labeler
}

func NewDynamicController(
	log logr.Logger,
	name string,
	kubeClient *dynamic.DynamicClient,
	handler func(context.Context, ctrl.Request) error,
) *DynamicController {
	rootLog := log
	logger := log.WithName("dynamic-controller")

	dc := &DynamicController{
		name:       name,
		kubeClient: kubeClient,
		queue: workqueue.NewNamedRateLimitingQueue(workqueue.NewMaxOfRateLimiter(
			workqueue.NewItemExponentialFailureRateLimiter(200*time.Millisecond, 1000*time.Second),
			// 10 qps, 100 bucket size.  This is only for retry speed and its only the overall factor (not per item)
			&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
		), "dynamic-controller-queue"),
		handler:            handler,
		informers:          map[schema.GroupVersionResource]dynamicinformer.DynamicSharedInformerFactory{},
		cancelFuncs:        map[schema.GroupVersionResource]context.CancelFunc{},
		log:                logger,
		rootLog:            rootLog,
		mu:                 sync.RWMutex{},
		listers:            map[schema.GroupVersionResource]dynamiclister.Lister{},
		workflowOperators:  map[schema.GroupVersionResource]*graphexec.Controller{},
		hasSyncedFunctions: map[schema.GroupVersionResource]func() bool{},
		symphonyLabeler: k8smetadata.NewSymphonyMetaLabeler(
			"dev",
			"pod-id",
		),
	}
	return dc
}

func (dc *DynamicController) AllInformerHaveSynced() bool {
	start := time.Now()
	dc.log.V(1).Info("Waiting for all informers to sync")
	defer func() {
		dc.log.V(1).Info("All informers have synced", "wait time", time.Since(start))
	}()

	dc.mu.RLock()
	defer dc.mu.RUnlock()
	if len(dc.hasSyncedFunctions) == 0 {
		return true
	}

	for _, hasSynced := range dc.hasSyncedFunctions {
		if !hasSynced() {
			return false
		}
	}
	return true
}

// Run the main goroutine responsible for watching and syncing jobs.
func (dc *DynamicController) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer dc.queue.ShutDown()

	dc.log.Info("Starting symphony dynamic controller", "name", dc.name)
	defer dc.log.Info("Shutting down symphony dynamic controller", "name", dc.name)

	if !cache.WaitForNamedCacheSync(dc.name, ctx.Done(), dc.AllInformerHaveSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, dc.worker, time.Second)
	}

	<-ctx.Done()
}

// worker runs a thread that dequeues CSRs, handles them, and marks them done.
func (dc *DynamicController) worker(ctx context.Context) {
	for dc.processNextWorkItem(ctx) {
	}
}

// ObjectIdentifiers is a struct that holds the namespaced key and the GVR of the object.
//
// Since we are handling all the resources using the same handlerFunc, we need to know
// what GVR we're dealing with - so that we can use the appropriate workflow operator.
type ObjectIdentifiers struct {
	NamespacedKey string
	GVR           schema.GroupVersionResource
}

// processNextWorkItem deals with one key off the queue.  It returns false when it's time to quit.
func (dc *DynamicController) processNextWorkItem(ctx context.Context) bool {
	item, quit := dc.queue.Get()
	if quit {
		return false
	}
	defer dc.queue.Done(item)

	itemUnwrapper := item.(ObjectIdentifiers)
	dc.log.V(1).Info("Processing next work item", "item", itemUnwrapper)

	err := dc.syncFunc(ctx, itemUnwrapper)
	if err != nil {
		if reqErr, ok := err.(*requeue.RequeueNeededAfter); ok {
			dc.log.V(1).Info("Requeue needed after error", "error", reqErr.Unwrap(), "after", reqErr.Duration())
			dc.queue.AddAfter(item, reqErr.Duration())
			// not fond error
		} else if strings.Contains(err.Error(), "not found") {
			dc.log.V(1).Info("Object not found in api-server", "error", err)
		} else {
			gvrKey := fmt.Sprintf("%s/%s/%s/%s", itemUnwrapper.GVR.Group, itemUnwrapper.GVR.Version, itemUnwrapper.GVR.Resource, itemUnwrapper.NamespacedKey)
			dc.log.V(1).Info("Error syncing item", "item", gvrKey, "error", err, "requeue", true)
			dc.queue.AddRateLimited(item)
		}
	}

	dc.log.V(1).Info("Successfully synced item", "item", itemUnwrapper, "forget", true)

	dc.queue.Forget(item)
	return true
}

func (dc *DynamicController) enqueueObject(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		dc.log.V(1).Info("Couldn't get key for object", "error", err)
		return
	}

	// obj is a instance of a defined resourcegroup, we do not know much about the contruct
	// so we enqueue two things:
	//   - the instance key (namespacedName)
	//   - the GVR of the instance (this will be usefull to know which handler to call)
	//
	// The reason we have so many handlers is because those handlers are compiled graphs
	// with a set of workflow steps that are specific to the resourcegroup.

	// Since we are using a dynamic informer/client, it is guaranteed that the object is
	// a pointer to unstructured.Unstructured.
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		dc.log.V(1).Info("Couldn't cast object to unstructured", "object", obj)
		return
	}

	// extract group and version from apiVersion
	apiVersion := u.GetAPIVersion()
	parts := strings.Split(apiVersion, "/")
	if len(parts) != 2 {
		dc.log.V(1).Info("Couldn't split apiVersion ???", "apiVersion", apiVersion)
		return
	}
	group := parts[0]
	version := parts[1]

	pluralKind := flect.Pluralize(strings.ToLower(u.GetKind()))
	objectIdentifiers := ObjectIdentifiers{
		NamespacedKey: key,
		GVR: schema.GroupVersionResource{
			Group:    group,
			Version:  version,
			Resource: pluralKind,
		},
	}

	dc.log.V(1).Info("Enqueueing object", "objectIdentifiers", objectIdentifiers)
	dc.queue.Add(objectIdentifiers)
}

func (dc *DynamicController) syncFunc(ctx context.Context, oi ObjectIdentifiers) error {
	//dc.mu.Lock()
	//defer dc.mu.Unlock()
	dc.log.V(1).Info("Syncing resourcegroup instance request", "gvr", oi.GVR, "namespacedKey", oi.NamespacedKey)

	startTime := time.Now()
	defer func() {
		dc.log.V(1).Info("Finished syncing resourcegroup instance request", "gvr", oi.GVR, "namespacedKey", oi.NamespacedKey, "timeElapsed", time.Since(startTime))
	}()

	wo, ok := dc.workflowOperators[oi.GVR]
	if !ok {
		return fmt.Errorf("no workflow operator found for GVR: %s", oi.GVR)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: oi.NamespacedKey}}
	return wo.Reconcile(ctx, req)
}

func (dc *DynamicController) Shutdown() {
	dc.queue.ShutDown()
	/* for _, informer := range dc.informers {
		informer.WaitForCacheSync(make(chan struct{}))
	} */
	for _, informer := range dc.informers {
		informer.Shutdown()
	}
}

// SafeRegisterGVK registers a new GVK to the informers map safely.
func (dc *DynamicController) SafeRegisterGVK(gvr schema.GroupVersionResource) {

	dc.mu.Lock()
	defer dc.mu.Unlock()
	if _, ok := dc.informers[gvr]; !ok {
		gvkInformer := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dc.kubeClient, 0, metav1.NamespaceAll, nil)

		informerEventsLog := dc.log.WithName("informer-events")
		gvkInformer.ForResource(gvr).Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				informerEventsLog.V(1).Info("Adding object to queue", "objectName", obj.(*unstructured.Unstructured).GetName(), "event", "add")
				dc.enqueueObject(obj)
			},
			UpdateFunc: func(old, new interface{}) {
				if isNil(new) {
					informerEventsLog.V(1).Info("Update event has no new object for update")
					return
				}
				if isNil(old) {
					informerEventsLog.V(1).Info("Update event has no old object for update")
					return
				}

				// skip if generation didn't change
				oldGeneration := old.(*unstructured.Unstructured).GetGeneration()
				newGeneration := new.(*unstructured.Unstructured).GetGeneration()
				if oldGeneration == newGeneration {
					informerEventsLog.V(1).Info("Skipping objects in which the metadata.generation didn't change", "objectName", new.(*unstructured.Unstructured).GetName(), "event", "update")
					return
				}

				informerEventsLog.V(1).Info("Adding object to queue", "objectName", new.(*unstructured.Unstructured).GetName(), "event", "update")
				dc.enqueueObject(new)
			},
			DeleteFunc: func(obj interface{}) {
				uu, ok := obj.(*unstructured.Unstructured)
				if !ok {
					tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
					if !ok {
						dc.log.V(1).Info("Couldn't get object from tombstone", "object", obj)
						return
					}
					uu, ok = tombstone.Obj.(*unstructured.Unstructured)
					if !ok {
						dc.log.V(1).Info("Tombstone contained object that is not an unstructured obj", "object", obj)
						return
					}
				}

				informerEventsLog.V(1).Info("Adding object to queue", "objectName", uu.GetName(), "event", "delete")
				dc.enqueueObject(uu)
			},
		})
		dc.informers[gvr] = gvkInformer
		_, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		dc.cancelFuncs[gvr] = cancel
		dc.hasSyncedFunctions[gvr] = gvkInformer.ForResource(gvr).Informer().HasSynced

		time.Sleep(1 * time.Second)

		dc.log.V(1).Info("Starting informer", "gvr", gvr)
		// go gvkInformer.Start(ctx.Done())
	}
	dc.log.Info("Finished safe registering GVR", "gvr", gvr)
}

func (dc *DynamicController) UnregisterGVK(gvr schema.GroupVersionResource) {
	dc.log.Info("Unregistering GVK", "gvr", gvr)
	dc.mu.Lock()
	defer dc.mu.Unlock()
	informer, ok := dc.informers[gvr]
	if ok {
		// dc.log.Info("Stopping informer", "gvr", gvr)
		dc.cancelFuncs[gvr]()
		informer.Shutdown()
		// dc.log.Info("Deleting informer", "gvr", gvr)
		delete(dc.informers, gvr)
	}
	dc.log.Info("Stop informers for GVK", "gvr", gvr)
}

func (dc *DynamicController) HotRestart() bool {
	// TODO: implement hot restart
	return true
}

func (dc *DynamicController) RegisterWorkflowOperator(
	ctx context.Context,
	rgResource *v1alpha1.ResourceGroup,
) (*resourcegroup.ResourceGroup, error) {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	dc.log.V(1).Info("Creating resourcegroup graph", "name", rgResource.Name)

	restConfig, err := kubernetes.NewRestConfig()
	if err != nil {
		return nil, err
	}

	builder, err := resourcegroup.NewResourceGroupBuilder(restConfig, celextractor.NewCELExpressionParser())
	if err != nil {
		return nil, err
	}

	processedRG, err := builder.NewResourceGroup(rgResource)
	if err != nil {
		return nil, err
	}

	gvr := k8smetadata.GVKtoGVR(processedRG.Instance.GroupVersionKind)
	dc.log.V(1).Info("Creating workflow operator", "gvr", gvr)

	rgLabeler := k8smetadata.NewResourceGroupLabeler(rgResource)
	graphExecLabeler, err := dc.symphonyLabeler.Merge(rgLabeler)
	if err != nil {
		return nil, err
	}

	wo := graphexec.New(dc.rootLog, gvr, processedRG, dc.kubeClient, graphExecLabeler)

	dc.workflowOperators[gvr] = wo
	return processedRG, nil
}

func (dc *DynamicController) validateResourceGroup(ctx context.Context, c *v1alpha1.Resource) error {
	return nil
}

func (dc *DynamicController) UnregisterWorkflowOperator(gvr schema.GroupVersionResource) {
	dc.log.Info("Unregistering workflow operator", "gvr", gvr)

	dc.mu.Lock()
	defer dc.mu.Unlock()
	delete(dc.workflowOperators, gvr)
}

func isNil(arg any) bool {
	if v := reflect.ValueOf(arg); !v.IsValid() || ((v.Kind() == reflect.Ptr ||
		v.Kind() == reflect.Interface ||
		v.Kind() == reflect.Slice ||
		v.Kind() == reflect.Map ||
		v.Kind() == reflect.Chan ||
		v.Kind() == reflect.Func) && v.IsNil()) {
		return true
	}
	return false
}
