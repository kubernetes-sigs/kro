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

package dynamiccontroller

import (
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// graphRevisionIndex indexes instances by every graph revision they reference:
// the revision they last reconciled against, the revision they are moving to,
// and the revision their pin annotation names. The RGD controller uses it to
// keep referenced revisions alive through retention GC.
const graphRevisionIndex = "graphRevision"

// newInstanceInformer builds a parent informer that keeps the revision fields
// of an instance's status in cache.
//
// Parent watches are normally metadata-only (see watch.Manager), but revision
// pinning needs status.graphRevision, which metadata informers never carry.
// The informer therefore lists/watches full objects and trims them on the way
// into the store, so the steady-state memory cost stays at roughly metadata
// size; only the wire cost grows.
func newInstanceInformer(client dynamic.Interface, gvr schema.GroupVersionResource, resync time.Duration) cache.SharedIndexInformer {
	informer := dynamicinformer.NewFilteredDynamicInformer(
		client, gvr, metav1.NamespaceAll, resync,
		cache.Indexers{
			cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
			graphRevisionIndex:   indexGraphRevisions,
		},
		nil,
	).Informer()
	if err := informer.SetTransform(trimInstance); err != nil {
		// SetTransform only fails on a started informer, and this one was just
		// created. Falling through keeps full objects in cache, which is
		// correct but larger.
		return informer
	}
	return informer
}

// trimInstance drops everything a parent watch does not need: the full spec and
// every author-projected status field. It is idempotent, as cache transforms
// may be applied to already-trimmed objects on resync.
func trimInstance(obj any) (any, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		// DeletedFinalStateUnknown and typed objects pass through untouched.
		return obj, nil
	}

	trimmed := map[string]any{}
	for _, key := range []string{"apiVersion", "kind", "metadata"} {
		if v, ok := u.Object[key]; ok {
			trimmed[key] = v
		}
	}
	if status, ok := u.Object["status"].(map[string]any); ok {
		kept := map[string]any{}
		for _, key := range []string{"graphRevision", "targetGraphRevision"} {
			if v, ok := status[key]; ok {
				kept[key] = v
			}
		}
		if len(kept) > 0 {
			trimmed["status"] = kept
		}
	}
	u.Object = trimmed
	return u, nil
}

// indexGraphRevisions returns the decimal revision numbers an instance
// references: its current revision, its target revision, and its pin
// annotation when that names a concrete revision.
func indexGraphRevisions(obj any) ([]string, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, nil
	}

	seen := make(map[int64]struct{}, 3)
	values := make([]string, 0, 3)
	add := func(rev int64) {
		if rev < 1 {
			return
		}
		if _, dup := seen[rev]; dup {
			return
		}
		seen[rev] = struct{}{}
		values = append(values, strconv.FormatInt(rev, 10))
	}

	if status, ok := u.Object["status"].(map[string]any); ok {
		for _, key := range []string{"graphRevision", "targetGraphRevision"} {
			if rev, ok := revisionNumber(status[key]); ok {
				add(rev)
			}
		}
	}
	if pin := u.GetAnnotations()[v1alpha1.InstanceGraphRevisionAnnotation]; pin != "" {
		if rev, err := strconv.ParseInt(pin, 10, 64); err == nil {
			add(rev)
		}
	}
	return values, nil
}

// revisionNumber reads a revision out of an unstructured status field. The
// API server returns int64, but a client-side decode of the same object can
// produce float64.
func revisionNumber(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	default:
		return 0, false
	}
}
