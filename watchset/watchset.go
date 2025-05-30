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

package watchset

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

// WatchSet manages watches for GroupKinds
type WatchSet struct {
	client dynamic.Interface
	// Map of GroupKind to check and trigger function pairs
	// map[GR]map[ID]callbackFns
	callbackMap sync.Map

	// watchMap is a map of GroupResource to bool
	// map[GR]watchEntry
	watchMap sync.Map

	// labelSelector is the label selector to use for the watch
	labelSelector string

	// RESTMapper for mapping GK to GVR
	restMapper meta.RESTMapper

	// cancelFn is a context.CancelFunc that stops the watchSet
	cancelFn  context.CancelFunc
	cancelCtx context.Context
}

// NewWatchGK creates a new WatchGK instance
func NewWatchSet(
	ctx context.Context,
	client dynamic.Interface,
	restMapper meta.RESTMapper,
	labelSelector string,
) *WatchSet {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancelFn := context.WithCancel(ctx)
	return &WatchSet{
		client:        client,
		restMapper:    restMapper,
		cancelFn:      cancelFn,
		cancelCtx:     ctx,
		labelSelector: labelSelector,
	}
}

type watchEntry struct {
	watcher watch.Interface
	stopCh  chan struct{}
}

// CheckFn is a function that checks if an event should trigger a callback
type CheckFn func(*unstructured.Unstructured) bool

// TriggerFn is a function that handles an event
type TriggerFn func(watch.Event)

// callbackFns represents a check and trigger function pair
type callbackFns struct {
	CheckFn   CheckFn
	TriggerFn TriggerFn
}

type Trigger struct {
	Channel   chan<- watch.Event
	TriggerFn TriggerFn
}

// UnregisterGVKNN unregisters a check and trigger function pair for a GroupVersionKind and NamespacedName
func (w *WatchSet) UnregisterGVKNNForParent(
	parent,
	child *unstructured.Unstructured,
) error {
	// Convert GroupKind to GroupVersionResource using RESTMapper
	// Watch should trigger for all versions of the GVK so ignoring version.
	gk := schema.GroupKind{
		Group: child.GetObjectKind().GroupVersionKind().Group,
		Kind:  child.GetKind(),
	}

	parentGVK := fmt.Sprintf("%s/%s", parent.GetAPIVersion(), parent.GetKind())
	parentNN := fmt.Sprintf("%s/%s", parent.GetNamespace(), parent.GetName())
	childGVK := fmt.Sprintf("%s/%s", child.GetAPIVersion(), child.GetKind())
	childNN := fmt.Sprintf("%s/%s", child.GetNamespace(), child.GetName())
	id := fmt.Sprintf("%s/%s/%s/%s", parentGVK, parentNN, childGVK, childNN)

	return w.UnregisterGK(id, gk)
}

// UnregisterGKForParent unregisters a check and trigger function pair for a GroupVersionKind and NamespacedName
func (w *WatchSet) UnregisterGKForParent(id string, parent *unstructured.Unstructured, gk schema.GroupKind) error {
	// Convert GroupKind to GroupVersionResource using RESTMapper
	// Watch should trigger for all versions of the GVK so ignoring version.
	mapping, err := w.restMapper.RESTMapping(gk)
	if err != nil {
		return err
	}
	gvr := schema.GroupVersionResource{
		Group:    mapping.GroupVersionKind.Group,
		Version:  mapping.GroupVersionKind.Version,
		Resource: mapping.Resource.Resource,
	}

	parentGVK := fmt.Sprintf("%s/%s", parent.GetAPIVersion(), parent.GetKind())
	parentNN := fmt.Sprintf("%s/%s", parent.GetNamespace(), parent.GetName())
	id = fmt.Sprintf("%s/%s/%s", parentGVK, parentNN, id)

	return w.UnregisterGVR(id, gvr)
}

func (w *WatchSet) UnregisterGVR(id string, gvr schema.GroupVersionResource) error {
	gr := schema.GroupResource{
		Group:    gvr.Group,
		Resource: gvr.Resource,
	}

	callbackMap, ok := w.callbackMap.Load(gr)
	if !ok {
		return nil
	}

	callbackMap.(*sync.Map).Delete(id)
	return nil
}

// UnregisterGK unregisters a check and trigger function pair for a GroupVersionKind
func (w *WatchSet) UnregisterGK(id string, gk schema.GroupKind) error {
	// Convert GroupKind to GroupVersionResource using RESTMapper
	// Watch should trigger for all versions of the GVK so ignoring version.
	mapping, err := w.restMapper.RESTMapping(gk)
	if err != nil {
		return err
	}
	gvr := schema.GroupVersionResource{
		Group:    mapping.GroupVersionKind.Group,
		Version:  mapping.GroupVersionKind.Version,
		Resource: mapping.Resource.Resource,
	}

	return w.UnregisterGVR(id, gvr)
}

func (w *WatchSet) RegisterGVR(id string, gvr schema.GroupVersionResource, checkFn CheckFn, triggerFn TriggerFn) error {
	gr := schema.GroupResource{
		Group:    gvr.Group,
		Resource: gvr.Resource,
	}

	callbackMap, _ := w.callbackMap.LoadOrStore(gr, &sync.Map{})
	callbackMap.(*sync.Map).Store(id, callbackFns{CheckFn: checkFn, TriggerFn: triggerFn})
	w.startWatch(gvr)
	return nil
}

// RegisterGK registers a check and trigger function pair for a GroupVersionKind
func (w *WatchSet) RegisterGK(id string, gk schema.GroupKind, checkFn CheckFn, triggerFn TriggerFn) error {
	// Convert GroupKind to GroupVersionResource using RESTMapper
	// Watch should trigger for all versions of the GVK so ignoring version.
	mapping, err := w.restMapper.RESTMapping(gk)
	if err != nil {
		return err
	}
	gvr := schema.GroupVersionResource{
		Group:    mapping.GroupVersionKind.Group,
		Version:  mapping.GroupVersionKind.Version,
		Resource: mapping.Resource.Resource,
	}

	return w.RegisterGVR(id, gvr, checkFn, triggerFn)
}

// RegisterGKForParent registers a check and trigger function pair for a GroupVersionKind for a parent object
func (w *WatchSet) RegisterGKForParent(
	id string,
	trigger Trigger,
	parent *unstructured.Unstructured,
	gk schema.GroupKind,
	checkFn CheckFn,
) error {
	mapping, err := w.restMapper.RESTMapping(gk)
	if err != nil {
		return err
	}
	gvr := schema.GroupVersionResource{
		Group:    mapping.GroupVersionKind.Group,
		Version:  mapping.GroupVersionKind.Version,
		Resource: mapping.Resource.Resource,
	}
	triggerFn := trigger.TriggerFn
	if triggerFn == nil {
		triggerFn = func(event watch.Event) {
			// Create a new event with the parent object
			parentEvent := watch.Event{
				// Any change in the child is treated as a Modified event for the parent.
				Type:   watch.Modified,
				Object: parent,
			}
			trigger.Channel <- parentEvent
		}
	}

	parentGVK := fmt.Sprintf("%s/%s", parent.GetAPIVersion(), parent.GetKind())
	parentNN := fmt.Sprintf("%s/%s", parent.GetNamespace(), parent.GetName())
	id = fmt.Sprintf("%s/%s/%s", parentGVK, parentNN, id)

	return w.RegisterGVR(id, gvr, checkFn, triggerFn)
}

// RegisterGVKNNForParent registers a watch for a specific GVK and NamespacedName for a parent object
func (w *WatchSet) RegisterGVKNNForParent(trigger Trigger, parent, child *unstructured.Unstructured) error {
	if trigger.Channel == nil && trigger.TriggerFn == nil {
		return fmt.Errorf("one of the trigger channel or trigger function must be provided")
	}

	gk := schema.GroupKind{
		Group: child.GetObjectKind().GroupVersionKind().Group,
		Kind:  child.GetKind(),
	}

	checkFn := func(obj *unstructured.Unstructured) bool {
		// Check if this object matches the child's name and namespace
		// We can depend on labels but labels could be deleted intentionally or accidentally.
		if obj.GetName() == child.GetName() && obj.GetNamespace() == child.GetNamespace() {
			return true
		}
		return false
	}

	triggerFn := trigger.TriggerFn
	if triggerFn == nil {
		triggerFn = func(event watch.Event) {
			// Create a new event with the parent object
			parentEvent := watch.Event{
				// Any change in the child is treated as a Modified event for the parent.
				Type:   watch.Modified,
				Object: parent,
			}
			trigger.Channel <- parentEvent
		}
	}
	parentGVK := fmt.Sprintf("%s/%s", parent.GetAPIVersion(), parent.GetKind())
	parentNN := fmt.Sprintf("%s/%s", parent.GetNamespace(), parent.GetName())
	childGVK := fmt.Sprintf("%s/%s", child.GetAPIVersion(), child.GetKind())
	childNN := fmt.Sprintf("%s/%s", child.GetNamespace(), child.GetName())
	id := fmt.Sprintf("%s/%s/%s/%s", parentGVK, parentNN, childGVK, childNN)

	return w.RegisterGK(id, gk, checkFn, triggerFn)
}

// startWatch starts a watch for a GroupResource
func (w *WatchSet) startWatch(gvr schema.GroupVersionResource) {
	gr := schema.GroupResource{
		Group:    gvr.Group,
		Resource: gvr.Resource,
	}

	// return if already watching
	_, existing := w.watchMap.LoadOrStore(gr, &watchEntry{watcher: nil, stopCh: make(chan struct{})})
	if existing {
		return
	}

	go func() {
		//log.Printf("Starting watch for %s", gr)
		errCount := 0
		defer w.watchMap.Delete(gr)
		for {
			select {
			case <-w.cancelCtx.Done():
				log.Printf("WatchSet stopped (context cancelled) for %s", gr)
				return
			default:
			}

			listOptions := metav1.ListOptions{}
			if w.labelSelector != "" {
				listOptions.LabelSelector = w.labelSelector
			}
			watcher, err := w.client.Resource(gvr).Watch(context.Background(), listOptions)
			if err != nil {
				// Log error and retry
				log.Printf("Error watching %s: %v", gr, err)
				errCount++
				if errCount > 100 {
					log.Printf("Giving up watching %s: %v", gr, err)
					break
				}
				time.Sleep(5 * time.Second)
				continue
			}

			w.watchMap.Store(gr, &watchEntry{watcher: watcher, stopCh: make(chan struct{})})

			// Process events
			for event := range watcher.ResultChan() {
				select {
				case <-w.cancelCtx.Done():
					log.Printf("WatchSet stopped (context cancelled) for %s", gr)
					return
				default:
				}
				if obj, ok := event.Object.(*unstructured.Unstructured); ok {
					if callbackMap, ok := w.callbackMap.Load(gr); ok {
						callbackMap.(*sync.Map).Range(func(key, value any) bool {
							if value.(callbackFns).CheckFn(obj) {
								value.(callbackFns).TriggerFn(event)
							}
							return true
						})
					}
				}
			}
		}
	}()
}

// StopWatch stops watching a GroupKind
func (w *WatchSet) Stop() {
	w.cancelFn()
}
