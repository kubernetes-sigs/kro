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
	"log"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type watchManagerUsingObject struct {
	watchset *WatchSet
	parent   *unstructured.Unstructured
	trigger  Trigger
}

// NewWatchManagerUsingObject creates a new WatchManager that can be used by the `watchset.NewDynamicInterface()`
// to register watches with `watchset`. Any resources get/create/update are automatically added to the watchset.
// Any time those resources are updated/deleted, the `parent` is notified. Watches are setup per GVK and a
// map[id]callbacks are used to demux a child event to the parent. This manager uses watchset to automatically
// construct the ID from the parent and child object.
// The `trigger` provides either a channel or a callback function where the parent event is sent.
func NewWatchManagerUsingObject(
	watchset *WatchSet,
	parent *unstructured.Unstructured,
	trigger Trigger,
) WatchSetManager {
	return &watchManagerUsingObject{
		watchset: watchset,
		parent:   parent,
		trigger:  trigger,
	}
}

func (w *watchManagerUsingObject) HandleGet(ctx context.Context, obj *unstructured.Unstructured) {
	w.HandlePatch(ctx, obj)
}

func (w *watchManagerUsingObject) HandleUpdate(ctx context.Context, obj *unstructured.Unstructured) {
	w.HandlePatch(ctx, obj)
}

func (w *watchManagerUsingObject) HandleCreate(ctx context.Context, obj *unstructured.Unstructured) {
	w.HandlePatch(ctx, obj)
}

func (w *watchManagerUsingObject) HandlePatch(ctx context.Context, obj *unstructured.Unstructured) {
	if obj == nil {
		return
	}

	// Register a watch for this specific resource
	if err := w.watchset.RegisterGVKNNForParent(w.trigger, w.parent, obj); err != nil {
		log.Printf("Failed to register watch for %s/%s: %v",
			obj.GetKind(),
			obj.GetName(),
			err)
	}
}

func (w *watchManagerUsingObject) HandleDelete(ctx context.Context, obj *unstructured.Unstructured) {
	if obj == nil {
		return
	}

	// Unregister the watch using the parent and child objects
	if err := w.watchset.UnregisterGVKNNForParent(w.parent, obj); err != nil {
		log.Printf("Failed to unregister watch for %s/%s: %v",
			obj.GetKind(),
			obj.GetName(),
			err)
	}
}
