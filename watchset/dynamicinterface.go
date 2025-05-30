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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

var _ dynamic.Interface = &dynamicInterface{}
var _ dynamic.NamespaceableResourceInterface = &namespaceableRsrcInterface{}
var _ dynamic.ResourceInterface = &rsrcInterface{}

type WatchSetManager interface {
	HandleCreate(ctx context.Context, obj *unstructured.Unstructured)
	HandleUpdate(ctx context.Context, obj *unstructured.Unstructured)
	HandlePatch(ctx context.Context, obj *unstructured.Unstructured)
	HandleDelete(ctx context.Context, obj *unstructured.Unstructured)
	HandleGet(ctx context.Context, obj *unstructured.Unstructured)
}

type dynamicInterface struct {
	dynamic.Interface
	watchsetMgr WatchSetManager
}

// NewDynamicInterface wraps a kubernetes dynamic interface client and adds watchset support.
// The `dynInterface` is the kubernetes dynamic interface that will be used wrapped and setup to invoke `watchsetMgr`
// to watch resources. `watchsetMgr` can be either a label based manager or an object based manager.
// The `watchsetMgr` is used to register watches for resources being created/updated/deleted by the `dynInterface`.
// The `watchsetMgr` can be created using the `watchset.NewWatchManagerUsingObject()` or
// `watchset.NewWatchManagerUsingLabels()` functions.
func NewDynamicInterface(dynInterface dynamic.Interface, watchsetMgr WatchSetManager) dynamic.Interface {
	return &dynamicInterface{
		Interface:   dynInterface,
		watchsetMgr: watchsetMgr,
	}
}

func (di *dynamicInterface) Resource(resource schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	ri := di.Interface.Resource(resource)
	return &namespaceableRsrcInterface{
		NamespaceableResourceInterface: ri,
		gvr:                            resource,
		di:                             di,
	}
}

type namespaceableRsrcInterface struct {
	gvr schema.GroupVersionResource
	dynamic.NamespaceableResourceInterface
	di *dynamicInterface
}

func (nri *namespaceableRsrcInterface) Namespace(ns string) dynamic.ResourceInterface {
	return &rsrcInterface{
		nri:               nri,
		ns:                ns,
		ResourceInterface: nri.NamespaceableResourceInterface.Namespace(ns),
	}
}

type rsrcInterface struct {
	ns string
	dynamic.ResourceInterface
	nri *namespaceableRsrcInterface
}

func (ri *rsrcInterface) Create(
	ctx context.Context,
	obj *unstructured.Unstructured,
	options metav1.CreateOptions,
	subresources ...string,
) (*unstructured.Unstructured, error) {
	u, err := ri.ResourceInterface.Create(ctx, obj, options, subresources...)
	if err == nil {
		ri.nri.di.watchsetMgr.HandleCreate(ctx, u)
	}
	return u, err
}

func (ri *rsrcInterface) Update(
	ctx context.Context,
	obj *unstructured.Unstructured,
	options metav1.UpdateOptions,
	subresources ...string,
) (*unstructured.Unstructured, error) {
	u, err := ri.ResourceInterface.Update(ctx, obj, options, subresources...)
	if err == nil {
		ri.nri.di.watchsetMgr.HandleUpdate(ctx, u)
	}
	return u, err
}

func (ri *rsrcInterface) UpdateStatus(
	ctx context.Context,
	obj *unstructured.Unstructured,
	options metav1.UpdateOptions,
) (*unstructured.Unstructured, error) {
	panic("method not implemented")
}
func (ri *rsrcInterface) Delete(
	ctx context.Context,
	name string,
	options metav1.DeleteOptions,
	subresources ...string,
) error {
	err := ri.ResourceInterface.Delete(ctx, name, options, subresources...)
	if err != nil {
		obj := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": ri.nri.gvr.GroupVersion().String(),
				"kind":       ri.nri.gvr.Resource,
				"metadata":   map[string]interface{}{"name": name, "namespace": ri.ns},
			},
		}
		ri.nri.di.watchsetMgr.HandleDelete(ctx, obj)
	}
	return err
}
func (ri *rsrcInterface) DeleteCollection(
	ctx context.Context,
	options metav1.DeleteOptions,
	listOptions metav1.ListOptions,
) error {
	panic("method not implemented")
}
func (ri *rsrcInterface) Get(
	ctx context.Context,
	name string,
	options metav1.GetOptions,
	subresources ...string,
) (*unstructured.Unstructured, error) {
	u, err := ri.ResourceInterface.Get(ctx, name, options, subresources...)
	if err == nil {
		ri.nri.di.watchsetMgr.HandleGet(ctx, u)
	}
	return u, err
}

func (ri *rsrcInterface) List(
	ctx context.Context,
	opts metav1.ListOptions,
) (*unstructured.UnstructuredList, error) {
	u, err := ri.ResourceInterface.List(ctx, opts)
	return u, err
}

func (ri *rsrcInterface) Watch(
	ctx context.Context,
	opts metav1.ListOptions,
) (watch.Interface, error) {
	panic("method not implemented")
}

func (ri *rsrcInterface) Patch(
	ctx context.Context,
	name string,
	pt types.PatchType,
	data []byte,
	options metav1.PatchOptions,
	subresources ...string,
) (*unstructured.Unstructured, error) {
	u, err := ri.ResourceInterface.Patch(ctx, name, pt, data, options, subresources...)
	if err == nil {
		ri.nri.di.watchsetMgr.HandlePatch(ctx, u)
	}
	return u, err
}

func (ri *rsrcInterface) Apply(
	ctx context.Context,
	name string,
	obj *unstructured.Unstructured,
	options metav1.ApplyOptions,
	subresources ...string,
) (*unstructured.Unstructured, error) {
	panic("method not implemented")
}

func (ri *rsrcInterface) ApplyStatus(
	ctx context.Context,
	name string,
	obj *unstructured.Unstructured,
	options metav1.ApplyOptions,
) (*unstructured.Unstructured, error) {
	panic("method not implemented")
}
