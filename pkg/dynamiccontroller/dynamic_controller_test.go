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
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// NOTE(a-hilaly): I'm just playing around with the dynamic controller code here
// trying to understand what are the parts that need to be mocked and what are the
// parts that need to be tested. I'll probably need to rewrite some parts of graphexec
// and dynamiccontroller to make this work.

func noopLogger() logr.Logger {
	opts := zap.Options{
		// Write to dev/null
		DestWriter: io.Discard,
	}
	logger := zap.New(zap.UseFlagOptions(&opts))
	return logger
}

func setupFakeClient(t testing.TB) (*fake.FakeMetadataClient, meta.RESTMapper) {
	t.Helper()
	scheme := runtime.NewScheme()
	assert.NoError(t, v1.AddMetaToScheme(scheme))
	gvk := schema.GroupVersionKind{Group: "test", Version: "v1", Kind: "Test"}
	obj := &v1.PartialObjectMetadata{}
	obj.SetGroupVersionKind(gvk)
	return fake.NewSimpleMetadataClient(scheme, obj), meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())
}

func TestDynamicController_WatchBehavior(t *testing.T) {
	deploy := &appsv1.Deployment{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-configmap",
			Namespace: "default",
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"test": []byte("bar"),
		},
	}

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, v1.AddMetaToScheme(scheme))

	pdeploy := &v1.PartialObjectMetadata{}
	pdeploy.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	pdeploy.SetName(deploy.Name)
	pdeploy.SetNamespace(deploy.Namespace)

	psecret := &v1.PartialObjectMetadata{}
	psecret.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Secret"})
	psecret.SetName(secret.Name)
	psecret.SetNamespace(secret.Namespace)

	client := fake.NewSimpleMetadataClient(scheme, pdeploy, psecret)
	deploymentUpdates := make(chan watch.Event, 10)
	client.PrependWatchReactor("deployments", func(action k8stesting.Action) (handled bool, ret watch.Interface, err error) {
		return true, watch.NewProxyWatcher(deploymentUpdates), nil
	})
	secretUpdates := make(chan watch.Event, 10)
	client.PrependWatchReactor("secrets", func(action k8stesting.Action) (handled bool, ret watch.Interface, err error) {
		return true, watch.NewProxyWatcher(secretUpdates), nil
	})

	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())
	deployGVK, err := apiutil.GVKForObject(deploy, scheme)
	require.NoError(t, err)
	mapper.Add(deployGVK, meta.RESTScopeNamespace)
	deployRESTMapping, err := mapper.RESTMapping(deployGVK.GroupKind(), deployGVK.Version)
	require.NoError(t, err)
	deployGVR := deployRESTMapping.Resource

	secretGVK, err := apiutil.GVKForObject(secret, scheme)
	require.NoError(t, err)
	mapper.Add(secretGVK, meta.RESTScopeNamespace)
	secretRESTMapping, err := mapper.RESTMapping(secretGVK.GroupKind(), secretGVK.Version)
	require.NoError(t, err)
	secretGVR := secretRESTMapping.Resource

	ctrl := NewDynamicController(noopLogger(), Config{
		Workers:              1,
		ResyncPeriod:         1 * time.Hour,
		QueueMaxRetries:      5,
		MinRetryDelay:        100 * time.Millisecond,
		MaxRetryDelay:        1 * time.Second,
		RateLimit:            10,
		BurstLimit:           100,
		QueueShutdownTimeout: 5 * time.Second,
	}, client, mapper)

	var mu sync.Mutex
	reconciled := make(map[string]int)

	handler := Handler(func(ctx context.Context, req controllerruntime.Request) error {
		mu.Lock()
		defer mu.Unlock()
		reconciled[req.Namespace+"/"+req.Name]++
		return nil
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() {
		if err := ctrl.Start(ctx); err != nil {
			require.ErrorIs(t, err, context.Canceled)
		}
	}()
	require.Eventually(t, func() bool {
		return ctrl.ctx != nil
	}, 5*time.Second, 100*time.Millisecond)

	// Register parent (ConfigMap) watching child (Secret)
	require.NoError(t, ctrl.Register(ctx, deployGVR, handler, secretGVR))

	// Simulate Secret update triggering ConfigMap reconciliation
	// first propagate a modification (like adding a finalizer, without adding any ownership)
	psecret.SetFinalizers(append(psecret.GetFinalizers(), "test"))
	secretUpdates <- watch.Event{
		Type:   watch.Modified,
		Object: psecret.DeepCopy(),
	}
	psecret.SetLabels(map[string]string{
		metadata.OwnedLabel:             "true",
		metadata.InstanceLabel:          deploy.GetName(),
		metadata.InstanceNamespaceLabel: deploy.GetNamespace(),
		metadata.InstanceGroupLabel:     deployGVK.Group,
		metadata.InstanceVersionLabel:   deployGVK.Version,
		metadata.InstanceKindLabel:      deployGVK.Kind,
	})
	secretUpdates <- watch.Event{
		Type:   watch.Modified,
		Object: psecret.DeepCopy(),
	}

	// Wait for initial reconciliation of parent
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return reconciled[fmt.Sprintf("%s/%s", deploy.GetNamespace(), deploy.GetName())] == 1
	}, 5*time.Second, 100*time.Millisecond)

	pdeploy = pdeploy.DeepCopy()
	pdeploy.Labels = map[string]string{
		"some-label": "some-value",
	}
	pdeploy.SetGeneration(deploy.GetGeneration() + 1)
	deploymentUpdates <- watch.Event{
		Type:   watch.Modified,
		Object: pdeploy.DeepCopy(),
	}
	// Wait for parent to reconcile again due to parent generation change
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return reconciled[fmt.Sprintf("%s/%s", deploy.GetNamespace(), deploy.GetName())] == 2
	}, 5*time.Second, 100*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.GreaterOrEqual(t, reconciled["default/test-configmap"], 2)

	_, registrationExists := ctrl.registrations[deployGVR]
	assert.True(t, registrationExists)
	_, watchExists := ctrl.watches[deployGVR]
	assert.True(t, watchExists)

	// Deregister and verify cleanup
	require.NoError(t, ctrl.Deregister(ctx, deployGVR))
	_, registrationExists = ctrl.registrations[deployGVR]
	assert.False(t, registrationExists)
	_, watchExists = ctrl.watches[deployGVR]
	assert.False(t, watchExists)
}

func TestNewDynamicController(t *testing.T) {
	logger := noopLogger()
	client, mapper := setupFakeClient(t)

	config := Config{
		Workers:         2,
		ResyncPeriod:    10 * time.Hour,
		QueueMaxRetries: 20,
		MinRetryDelay:   200 * time.Millisecond,
		MaxRetryDelay:   1000 * time.Second,
		RateLimit:       10,
		BurstLimit:      100,
	}

	dc := NewDynamicController(logger, config, client, mapper)

	assert.NotNil(t, dc)
	assert.Equal(t, config, dc.config)
	assert.NotNil(t, dc.queue)
}

func TestRegisterAndUnregisterGVK(t *testing.T) {
	logger := noopLogger()
	client, mapper := setupFakeClient(t)

	config := Config{
		Workers:         1,
		ResyncPeriod:    1 * time.Second,
		QueueMaxRetries: 5,
		MinRetryDelay:   200 * time.Millisecond,
		MaxRetryDelay:   1000 * time.Second,
		RateLimit:       10,
		BurstLimit:      100,
	}

	dc := NewDynamicController(logger, config, client, mapper)

	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	// Create a context with cancel for running the controller
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Start the controller in a goroutine
	go func() {
		err := dc.Start(ctx)
		require.NoError(t, err)
	}()

	// Give the controller time to start
	time.Sleep(1 * time.Second)

	handlerFunc := Handler(func(ctx context.Context, req controllerruntime.Request) error {
		return nil
	})

	// Register GVK
	err := dc.Register(t.Context(), gvr, handlerFunc)
	require.NoError(t, err)

	_, exists := dc.registrations[gvr]
	assert.True(t, exists)

	// Try to register again (should not fail)
	err = dc.Register(t.Context(), gvr, handlerFunc)
	assert.NoError(t, err)

	// Unregister GVK
	shutdownContext, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err = dc.Deregister(shutdownContext, gvr)
	require.NoError(t, err)

	_, exists = dc.registrations[gvr]
	assert.False(t, exists)
}

func TestEnqueueObject(t *testing.T) {
	logger := noopLogger()
	client, mapper := setupFakeClient(t)

	dc := NewDynamicController(logger, Config{MinRetryDelay: 200 * time.Millisecond,
		MaxRetryDelay: 1000 * time.Second,
		RateLimit:     10,
		BurstLimit:    100}, client, mapper)

	obj := &v1.PartialObjectMetadata{}
	obj.SetName("test-object")
	obj.SetNamespace("default")
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "test", Version: "v1", Kind: "Test"})

	dc.enqueueParent(schema.GroupVersionResource{Group: "group", Version: "version", Resource: "resource"}, obj, "add")

	assert.Equal(t, 1, dc.queue.Len())
}

func TestInstanceUpdatePolicy(t *testing.T) {
	logger := noopLogger()

	scheme := runtime.NewScheme()
	assert.NoError(t, v1.AddMetaToScheme(scheme))
	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}
	gvk := schema.GroupVersionKind{Group: "test", Version: "v1", Kind: "Test"}
	scheme.AddKnownTypeWithName(gvk, &v1.PartialObjectMetadata{})

	objs := make(map[string]runtime.Object)

	obj1 := &v1.PartialObjectMetadata{}
	obj1.SetGroupVersionKind(gvk)
	obj1.SetNamespace("default")
	obj1.SetName("test-object-1")
	objs[obj1.GetNamespace()+"/"+obj1.GetName()] = obj1

	obj2 := &v1.PartialObjectMetadata{}
	obj2.SetGroupVersionKind(gvk)
	obj2.SetNamespace("test-namespace")
	obj2.SetName("test-object-2")
	objs[obj2.GetNamespace()+"/"+obj2.GetName()] = obj2

	client := fake.NewSimpleMetadataClient(scheme, obj1, obj2)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	dc := NewDynamicController(logger, Config{}, client, mapper)
	dc.ctx = t.Context() // simulate a start through dc.Run

	handlerFunc := Handler(func(ctx context.Context, req controllerruntime.Request) error {
		fmt.Println("reconciling instance", req)
		return nil
	})

	// simulate initial creation of the resource graph
	err := dc.Register(t.Context(), gvr, handlerFunc)
	assert.NoError(t, err)

	// simulate reconciling the instances
	for dc.queue.Len() > 0 {
		item, _ := dc.queue.Get()
		dc.queue.Done(item)
		dc.queue.Forget(item)
	}

	// simulate updating the resource graph
	err = dc.Register(t.Context(), gvr, handlerFunc)
	assert.NoError(t, err)

	// check if the expected objects are queued
	assert.Equal(t, dc.queue.Len(), 2)
	for dc.queue.Len() > 0 {
		name, _ := dc.queue.Get()
		_, ok := objs[name.String()]
		assert.True(t, ok)
	}
}
