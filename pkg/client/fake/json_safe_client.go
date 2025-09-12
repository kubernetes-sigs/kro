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

package fake

import (
	"context"
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

// JSONSafeDynamicClient wraps a fake dynamic client and performs JSON marshal/unmarshal
// to sanitize data before passing it to the underlying client, preventing deep copy issues
// with pointer fields
type JSONSafeDynamicClient struct {
	client dynamic.Interface
}

// NewJSONSafeDynamicClient creates a new JSON-safe wrapper around a dynamic client
func NewJSONSafeDynamicClient(client dynamic.Interface) *JSONSafeDynamicClient {
	return &JSONSafeDynamicClient{
		client: client,
	}
}

// Resource returns a resource interface for the given resource
func (j *JSONSafeDynamicClient) Resource(resource schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &jsonSafeNamespaceableResourceInterface{
		resource: j.client.Resource(resource),
	}
}

// jsonSafeNamespaceableResourceInterface wraps a NamespaceableResourceInterface
type jsonSafeNamespaceableResourceInterface struct {
	resource dynamic.NamespaceableResourceInterface
}

func (j *jsonSafeNamespaceableResourceInterface) Namespace(namespace string) dynamic.ResourceInterface {
	return &jsonSafeResourceInterface{
		resource: j.resource.Namespace(namespace),
	}
}

func (j *jsonSafeNamespaceableResourceInterface) Create(ctx context.Context, obj *unstructured.Unstructured, options metav1.CreateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	sanitized, err := deepCopyUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return j.resource.Create(ctx, sanitized, options, subresources...)
}

func (j *jsonSafeNamespaceableResourceInterface) Update(ctx context.Context, obj *unstructured.Unstructured, options metav1.UpdateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	sanitized, err := deepCopyUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return j.resource.Update(ctx, sanitized, options, subresources...)
}

func (j *jsonSafeNamespaceableResourceInterface) UpdateStatus(ctx context.Context, obj *unstructured.Unstructured, options metav1.UpdateOptions) (*unstructured.Unstructured, error) {
	sanitized, err := deepCopyUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return j.resource.UpdateStatus(ctx, sanitized, options)
}

func (j *jsonSafeNamespaceableResourceInterface) Delete(ctx context.Context, name string, options metav1.DeleteOptions, subresources ...string) error {
	return j.resource.Delete(ctx, name, options, subresources...)
}

func (j *jsonSafeNamespaceableResourceInterface) DeleteCollection(ctx context.Context, options metav1.DeleteOptions, listOptions metav1.ListOptions) error {
	return j.resource.DeleteCollection(ctx, options, listOptions)
}

func (j *jsonSafeNamespaceableResourceInterface) Get(ctx context.Context, name string, options metav1.GetOptions, subresources ...string) (*unstructured.Unstructured, error) {
	return j.resource.Get(ctx, name, options, subresources...)
}

func (j *jsonSafeNamespaceableResourceInterface) List(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	return j.resource.List(ctx, opts)
}

func (j *jsonSafeNamespaceableResourceInterface) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	return j.resource.Watch(ctx, opts)
}

func (j *jsonSafeNamespaceableResourceInterface) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, options metav1.PatchOptions, subresources ...string) (*unstructured.Unstructured, error) {
	return j.resource.Patch(ctx, name, pt, data, options, subresources...)
}

func (j *jsonSafeNamespaceableResourceInterface) Apply(ctx context.Context, name string, obj *unstructured.Unstructured, options metav1.ApplyOptions, subresources ...string) (*unstructured.Unstructured, error) {
	sanitized, err := deepCopyUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return j.resource.Apply(ctx, name, sanitized, options, subresources...)
}

func (j *jsonSafeNamespaceableResourceInterface) ApplyStatus(ctx context.Context, name string, obj *unstructured.Unstructured, options metav1.ApplyOptions) (*unstructured.Unstructured, error) {
	sanitized, err := deepCopyUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return j.resource.ApplyStatus(ctx, name, sanitized, options)
}

// jsonSafeResourceInterface wraps a ResourceInterface (for namespaced resources)
type jsonSafeResourceInterface struct {
	resource dynamic.ResourceInterface
}

func (j *jsonSafeResourceInterface) Create(ctx context.Context, obj *unstructured.Unstructured, options metav1.CreateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	sanitized, err := deepCopyUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return j.resource.Create(ctx, sanitized, options, subresources...)
}

func (j *jsonSafeResourceInterface) Update(ctx context.Context, obj *unstructured.Unstructured, options metav1.UpdateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	sanitized, err := deepCopyUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return j.resource.Update(ctx, sanitized, options, subresources...)
}

func (j *jsonSafeResourceInterface) UpdateStatus(ctx context.Context, obj *unstructured.Unstructured, options metav1.UpdateOptions) (*unstructured.Unstructured, error) {
	sanitized, err := deepCopyUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return j.resource.UpdateStatus(ctx, sanitized, options)
}

func (j *jsonSafeResourceInterface) Delete(ctx context.Context, name string, options metav1.DeleteOptions, subresources ...string) error {
	return j.resource.Delete(ctx, name, options, subresources...)
}

func (j *jsonSafeResourceInterface) DeleteCollection(ctx context.Context, options metav1.DeleteOptions, listOptions metav1.ListOptions) error {
	return j.resource.DeleteCollection(ctx, options, listOptions)
}

func (j *jsonSafeResourceInterface) Get(ctx context.Context, name string, options metav1.GetOptions, subresources ...string) (*unstructured.Unstructured, error) {
	return j.resource.Get(ctx, name, options, subresources...)
}

func (j *jsonSafeResourceInterface) List(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	return j.resource.List(ctx, opts)
}

func (j *jsonSafeResourceInterface) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	return j.resource.Watch(ctx, opts)
}

func (j *jsonSafeResourceInterface) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, options metav1.PatchOptions, subresources ...string) (*unstructured.Unstructured, error) {
	return j.resource.Patch(ctx, name, pt, data, options, subresources...)
}

func (j *jsonSafeResourceInterface) Apply(ctx context.Context, name string, obj *unstructured.Unstructured, options metav1.ApplyOptions, subresources ...string) (*unstructured.Unstructured, error) {
	sanitized, err := deepCopyUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return j.resource.Apply(ctx, name, sanitized, options, subresources...)
}

func (j *jsonSafeResourceInterface) ApplyStatus(ctx context.Context, name string, obj *unstructured.Unstructured, options metav1.ApplyOptions) (*unstructured.Unstructured, error) {
	sanitized, err := deepCopyUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return j.resource.ApplyStatus(ctx, name, sanitized, options)
}

// deepCopyUnstructured performs JSON marshal/unmarshal to sanitize unstructured data
// This removes pointer references and makes the data safe for fake client deep copying
func deepCopyUnstructured(obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	// Marshal to JSON
	jsonData, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}

	// Unmarshal back to unstructured
	var sanitized unstructured.Unstructured
	if err := json.Unmarshal(jsonData, &sanitized); err != nil {
		return nil, err
	}

	return &sanitized, nil
}
