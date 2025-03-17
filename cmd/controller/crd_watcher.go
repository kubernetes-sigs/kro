package controllers

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// CRDWatcher monitors changes to Custom Resource Definitions (CRDs).
type CRDWatcher struct {
	clientset kubernetes.Interface
	queue     workqueue.RateLimitingInterface
	ctx       context.Context
}

// NewCRDWatcher initializes and returns a CRDWatcher instance.
func NewCRDWatcher(clientset kubernetes.Interface, queue workqueue.RateLimitingInterface, ctx context.Context) *CRDWatcher {
	return &CRDWatcher{
		clientset: clientset,
		queue:     queue,
		ctx:       ctx,
	}
}

// Start initializes the CRD informer and starts watching for changes.
func (w *CRDWatcher) Start() {
	factory := externalversions.NewSharedInformerFactoryWithOptions(
		w.clientset,
		10*time.Minute,
		externalversions.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = "managed-by=kro"
		}),
	)

	crdInformer := factory.Apiextensions().V1().CustomResourceDefinitions().Informer()

	crdInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    w.enqueueCRD("add"),
		UpdateFunc: w.handleUpdate,
		DeleteFunc: w.enqueueCRD("delete"),
	})

	stopCh := make(chan struct{})
	defer close(stopCh)

	go factory.Start(stopCh)

	if !cache.WaitForCacheSync(stopCh, crdInformer.HasSynced) {
		fmt.Println("Error: Timed out waiting for CRD informer to sync")
		return
	}

	fmt.Println("CRD Watcher started successfully.")

	<-w.ctx.Done()
	fmt.Println("Shutting down CRD watcher...")
}

// enqueueCRD adds a CRD key to the work queue.
func (w *CRDWatcher) enqueueCRD(eventType string) func(obj interface{}) {
	return func(obj interface{}) {
		key, err := cache.MetaNamespaceKeyFunc(obj)
		if err != nil {
			fmt.Printf("Error generating key for %s event: %v\n", eventType, err)
			return
		}
		fmt.Printf("CRD %s detected: %s\n", eventType, key)
		w.queue.AddRateLimited(key)
	}
}

// handleUpdate processes CRD updates only if the resource version changes.
func (w *CRDWatcher) handleUpdate(oldObj, newObj interface{}) {
	oldCRD := oldObj.(metav1.Object)
	newCRD := newObj.(metav1.Object)

	if oldCRD.GetResourceVersion() == newCRD.GetResourceVersion() {
		// No significant change, skip processing
		return
	}

	w.enqueueCRD("update")(newObj)
}
