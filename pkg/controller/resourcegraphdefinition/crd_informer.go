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

package resourcegraphdefinition

import (
	"time"

	"github.com/go-logr/logr"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsinformers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	apiextensionslisters "k8s.io/apiextensions-apiserver/pkg/client/listers/apiextensions/v1"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	kroclient "github.com/kubernetes-sigs/kro/pkg/client"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// CRDInformer watches CRDs and triggers RGD reconciliation when CRDs become established
// or when their spec changes (for drift correction).
type CRDInformer struct {
	log        logr.Logger
	informer   cache.SharedIndexInformer
	lister     apiextensionslisters.CustomResourceDefinitionLister
	eventsChan chan<- event.GenericEvent
	stopCh     chan struct{}
}

// NewCRDInformer creates a new CRD informer that watches for CRD establishment and spec changes.
func NewCRDInformer(
	log logr.Logger,
	clientSet kroclient.SetInterface,
	eventsChan chan<- event.GenericEvent,
	resyncPeriod time.Duration,
) *CRDInformer {
	factory := apiextensionsinformers.NewSharedInformerFactory(
		clientSet.APIExtensions(),
		resyncPeriod,
	)

	crdInformer := factory.Apiextensions().V1().CustomResourceDefinitions()

	return &CRDInformer{
		log:        log.WithName("crd-informer"),
		informer:   crdInformer.Informer(),
		lister:     crdInformer.Lister(),
		eventsChan: eventsChan,
		stopCh:     make(chan struct{}),
	}
}

// Start starts the CRD informer and registers event handlers.
func (c *CRDInformer) Start() {
	c.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onAdd,
		UpdateFunc: c.onUpdate,
		DeleteFunc: c.onDelete,
	})

	go c.informer.Run(c.stopCh)
}

// Stop stops the CRD informer.
func (c *CRDInformer) Stop() {
	close(c.stopCh)
}

// HasSynced returns true if the informer has synced.
func (c *CRDInformer) HasSynced() bool {
	return c.informer.HasSynced()
}

// onAdd handles CRD creation events.
func (c *CRDInformer) onAdd(obj interface{}) {
	crd, ok := obj.(*extv1.CustomResourceDefinition)
	if !ok {
		return
	}

	// Only care about KRO-owned CRDs
	if !metadata.IsKROOwned(crd) {
		return
	}

	c.log.V(1).Info("CRD added", "name", crd.Name, "established", kroclient.IsEstablished(crd))

	// If CRD is already established on add, trigger reconciliation
	if kroclient.IsEstablished(crd) {
		c.enqueueRGD(crd)
	}
}

// onUpdate handles CRD update events.
func (c *CRDInformer) onUpdate(oldObj, newObj interface{}) {
	oldCRD, ok := oldObj.(*extv1.CustomResourceDefinition)
	if !ok {
		return
	}
	newCRD, ok := newObj.(*extv1.CustomResourceDefinition)
	if !ok {
		return
	}

	// Only care about KRO-owned CRDs
	if !metadata.IsKROOwned(newCRD) {
		return
	}

	wasEstablished := kroclient.IsEstablished(oldCRD)
	isEstablished := kroclient.IsEstablished(newCRD)

	// Trigger when CRD becomes established (for initial activation)
	if !wasEstablished && isEstablished {
		c.log.Info("CRD became established", "name", newCRD.Name)
		c.enqueueRGD(newCRD)
		return
	}

	// Trigger on spec changes when CRD is established (for drift correction)
	if isEstablished && oldCRD.Generation != newCRD.Generation {
		c.log.Info("CRD spec changed", "name", newCRD.Name, "generation", newCRD.Generation)
		c.enqueueRGD(newCRD)
		return
	}
}

// onDelete handles CRD deletion events.
func (c *CRDInformer) onDelete(obj interface{}) {
	// We don't need to handle CRD deletion - the RGD finalizer handles cleanup
}

// enqueueRGD sends an event to trigger RGD reconciliation.
func (c *CRDInformer) enqueueRGD(crd *extv1.CustomResourceDefinition) {
	rgdName, ok := crd.Labels[metadata.ResourceGraphDefinitionNameLabel]
	if !ok {
		c.log.V(1).Info("CRD missing RGD name label", "crd", crd.Name)
		return
	}

	c.log.Info("Triggering RGD reconciliation", "crd", crd.Name, "rgd", rgdName)

	// Create a minimal RGD object for the event
	rgd := &v1alpha1.ResourceGraphDefinition{}
	rgd.SetName(rgdName)

	c.eventsChan <- event.GenericEvent{Object: rgd}
}
