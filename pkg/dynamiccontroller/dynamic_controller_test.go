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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/kubernetes-sigs/kro/pkg/dynamiccontroller/internal"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
)

// NOTE(a-hilaly): I'm just playing around with the dynamic controller code here
// trying to understand what are the parts that need to be mocked and what are the
// parts that need to be tested. I'll probably need to rewrite some parts of graphexec
// and dynamiccontroller to make this work.

func noopLogger() logr.Logger {
	opts := zap.Options{
		DestWriter: io.Discard,
	}
	return zap.New(zap.UseFlagOptions(&opts))
}

func testConfig() Config {
	return Config{
		Workers:         1,
		ResyncPeriod:    1 * time.Hour,
		QueueMaxRetries: 3,
		MinRetryDelay:   10 * time.Millisecond,
		MaxRetryDelay:   100 * time.Millisecond,
		RateLimit:       100,
		BurstLimit:      1000,
	}
}

type erroringInformer struct {
	cache.SharedIndexInformer
	removeErr error
}

func (e *erroringInformer) RemoveEventHandler(cache.ResourceEventHandlerRegistration) error {
	return e.removeErr
}

type fakeRegistration struct{}

func (fakeRegistration) HasSynced() bool { return true }

type capturingInformer struct {
	cache.SharedIndexInformer
	handler cache.ResourceEventHandler
}

func (c *capturingInformer) AddEventHandler(h cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	c.handler = h
	return fakeRegistration{}, nil
}

func (c *capturingInformer) HasSynced() bool { return true }

func (c *capturingInformer) Run(stopCh <-chan struct{}) {}

type blockingQueue struct {
	workqueue.TypedRateLimitingInterface[ObjectIdentifiers]
	blockCh chan struct{}
}

func (b *blockingQueue) ShutDown() {
	<-b.blockCh
	b.TypedRateLimitingInterface.ShutDown()
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
		return ctrl.ctx.Load() != nil
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

	dc := NewDynamicController(logger, Config{
		MinRetryDelay: 200 * time.Millisecond,
		MaxRetryDelay: 1000 * time.Second,
		RateLimit:     10,
		BurstLimit:    100,
	}, client, mapper)

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
	ctx := t.Context()
	dc.ctx.Store(&ctx) // simulate a start through dc.Run

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

func TestUpdateFunc_GenerationFiltering(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)

	parentGVR := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	oldObj := &v1.PartialObjectMetadata{}
	oldObj.SetName("test-obj")
	oldObj.SetNamespace("default")
	oldObj.SetGeneration(1)

	newObj := oldObj.DeepCopy()
	newObj.SetGeneration(1)

	dc.updateFunc(parentGVR, oldObj, newObj)
	assert.Equal(t, 0, dc.queue.Len(), "same generation should not enqueue")

	newObj.SetGeneration(2)
	dc.updateFunc(parentGVR, oldObj, newObj)
	assert.Equal(t, 1, dc.queue.Len(), "different generation should enqueue")

	item, _ := dc.queue.Get()
	dc.queue.Done(item)
	dc.queue.Forget(item)

	dc.updateFunc(parentGVR, struct{}{}, newObj)
	assert.Equal(t, 0, dc.queue.Len(), "invalid old object should not enqueue")

	dc.updateFunc(parentGVR, oldObj, struct{}{})
	assert.Equal(t, 0, dc.queue.Len(), "invalid new object should not enqueue")

	oldObj.SetLabels(map[string]string{
		metadata.InstanceSuspendReconciliationLabel: "disabled",
	})

	dc.updateFunc(parentGVR, oldObj, newObj)
	assert.Equal(t, 1, dc.queue.Len(), "should enqueue if suspend label was removed")
}

func TestStart_AlreadyRunning(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	startedCh := make(chan struct{})
	go func() {
		close(startedCh)
		_ = dc.Start(ctx)
	}()

	<-startedCh
	require.Eventually(t, func() bool { return dc.ctx.Load() != nil }, 2*time.Second, 10*time.Millisecond)

	err := dc.Start(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already running")
}

func TestEnqueueParent_InvalidObject(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)

	parentGVR := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	dc.enqueueParent(parentGVR, struct{}{}, "add")
	assert.Equal(t, 0, dc.queue.Len(), "invalid object should not enqueue")

	dc.enqueueParent(parentGVR, nil, "add")
	assert.Equal(t, 0, dc.queue.Len(), "nil object should not enqueue")
}

func TestProcessNextWorkItem_RequeueBehaviors(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	parentGVR := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}
	oi := ObjectIdentifiers{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
		GVR:            parentGVR,
	}

	t.Run("no handler registered", func(t *testing.T) {
		dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)
		dc.queue.Add(oi)
		assert.Equal(t, 1, dc.queue.Len())

		result := dc.processNextWorkItem(t.Context())
		assert.True(t, result)
		assert.Equal(t, 0, dc.queue.Len())
	})

	t.Run("handler returns nil", func(t *testing.T) {
		dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)
		dc.handlers.Store(parentGVR, Handler(func(ctx context.Context, req controllerruntime.Request) error {
			return nil
		}))

		dc.queue.Add(oi)
		result := dc.processNextWorkItem(t.Context())
		assert.True(t, result)
		assert.Equal(t, 0, dc.queue.Len())
	})

	t.Run("handler returns NoRequeue", func(t *testing.T) {
		dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)
		dc.handlers.Store(parentGVR, Handler(func(ctx context.Context, req controllerruntime.Request) error {
			return requeue.None(fmt.Errorf("terminal error"))
		}))

		dc.queue.Add(oi)
		result := dc.processNextWorkItem(t.Context())
		assert.True(t, result)
		assert.Equal(t, 0, dc.queue.Len())
	})

	t.Run("handler returns RequeueNeeded", func(t *testing.T) {
		dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)
		callCount := 0
		dc.handlers.Store(parentGVR, Handler(func(ctx context.Context, req controllerruntime.Request) error {
			callCount++
			if callCount == 1 {
				return requeue.Needed(fmt.Errorf("need requeue"))
			}
			return nil
		}))

		dc.queue.Add(oi)

		result := dc.processNextWorkItem(t.Context())
		assert.True(t, result)
		assert.Equal(t, 1, dc.queue.Len())

		result = dc.processNextWorkItem(t.Context())
		assert.True(t, result)
		assert.Equal(t, 0, dc.queue.Len())
		assert.Equal(t, 2, callCount)
	})

	t.Run("handler returns RequeueNeededAfter", func(t *testing.T) {
		dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)
		dc.handlers.Store(parentGVR, Handler(func(ctx context.Context, req controllerruntime.Request) error {
			return requeue.NeededAfter(fmt.Errorf("retry later"), 50*time.Millisecond)
		}))

		dc.queue.Add(oi)
		result := dc.processNextWorkItem(t.Context())
		assert.True(t, result)
		assert.Equal(t, 0, dc.queue.Len())

		time.Sleep(100 * time.Millisecond)
		assert.Equal(t, 1, dc.queue.Len())
	})

	t.Run("handler returns NotFound error", func(t *testing.T) {
		dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)
		dc.handlers.Store(parentGVR, Handler(func(ctx context.Context, req controllerruntime.Request) error {
			return apierrors.NewNotFound(schema.GroupResource{Group: "test", Resource: "tests"}, "test")
		}))

		dc.queue.Add(oi)
		result := dc.processNextWorkItem(t.Context())
		assert.True(t, result)
		assert.Equal(t, 0, dc.queue.Len())
	})

	t.Run("handler returns generic error rate limited then dropped", func(t *testing.T) {
		cfg := testConfig()
		cfg.QueueMaxRetries = 2
		cfg.MinRetryDelay = 1 * time.Millisecond
		cfg.MaxRetryDelay = 5 * time.Millisecond
		cfg.RateLimit = 1000
		dc := NewDynamicController(noopLogger(), cfg, client, mapper)
		dc.handlers.Store(parentGVR, Handler(func(ctx context.Context, req controllerruntime.Request) error {
			return fmt.Errorf("transient error")
		}))

		dc.queue.Add(oi)

		// First attempt (NumRequeues=0): should be requeued with rate limiting
		result := dc.processNextWorkItem(t.Context())
		assert.True(t, result)
		require.Eventually(t, func() bool {
			return dc.queue.Len() == 1
		}, 100*time.Millisecond, 1*time.Millisecond, "item should be requeued after first failure")

		// Second attempt (NumRequeues=1): should be requeued again
		result = dc.processNextWorkItem(t.Context())
		assert.True(t, result)
		require.Eventually(t, func() bool {
			return dc.queue.Len() == 1
		}, 100*time.Millisecond, 1*time.Millisecond, "item should be requeued after second failure")

		// Third attempt (NumRequeues=2 >= QueueMaxRetries): should be dropped
		result = dc.processNextWorkItem(t.Context())
		assert.True(t, result)
		// Give a brief moment for any async operations, then verify item was dropped
		time.Sleep(10 * time.Millisecond)
		assert.Equal(t, 0, dc.queue.Len(), "item should be dropped after max retries")
	})

	t.Run("queue shutdown returns false", func(t *testing.T) {
		dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)
		dc.queue.ShutDown()
		result := dc.processNextWorkItem(t.Context())
		assert.False(t, result)
	})
}

func TestChildHandler_LabelFiltering(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1.AddMetaToScheme(scheme))

	parentGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	parentGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	childGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}

	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())
	mapper.Add(parentGVK, meta.RESTScopeNamespace)

	client := fake.NewSimpleMetadataClient(scheme)

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)

	handler, err := dc.handlerForChildGVR(parentGVR, childGVR)
	require.NoError(t, err)

	funcs := handler.(cache.ResourceEventHandlerFuncs)

	t.Run("missing owned label", func(t *testing.T) {
		obj := &v1.PartialObjectMetadata{}
		obj.SetName("child")
		obj.SetNamespace("default")
		obj.SetLabels(map[string]string{
			metadata.InstanceLabel:          "parent",
			metadata.InstanceNamespaceLabel: "default",
		})

		funcs.AddFunc(obj)
		assert.Equal(t, 0, dc.queue.Len())
	})

	t.Run("owned label not true", func(t *testing.T) {
		obj := &v1.PartialObjectMetadata{}
		obj.SetName("child")
		obj.SetNamespace("default")
		obj.SetLabels(map[string]string{
			metadata.OwnedLabel:             "false",
			metadata.InstanceLabel:          "parent",
			metadata.InstanceNamespaceLabel: "default",
		})

		funcs.AddFunc(obj)
		assert.Equal(t, 0, dc.queue.Len())
	})

	t.Run("missing instance label", func(t *testing.T) {
		obj := &v1.PartialObjectMetadata{}
		obj.SetName("child")
		obj.SetNamespace("default")
		obj.SetLabels(map[string]string{
			metadata.OwnedLabel:             "true",
			metadata.InstanceNamespaceLabel: "default",
		})

		funcs.AddFunc(obj)
		assert.Equal(t, 0, dc.queue.Len())
	})

	t.Run("missing instance namespace label", func(t *testing.T) {
		obj := &v1.PartialObjectMetadata{}
		obj.SetName("child")
		obj.SetNamespace("default")
		obj.SetLabels(map[string]string{
			metadata.OwnedLabel:    "true",
			metadata.InstanceLabel: "parent",
		})

		funcs.AddFunc(obj)
		assert.Equal(t, 0, dc.queue.Len())
	})

	t.Run("wrong GVK labels", func(t *testing.T) {
		obj := &v1.PartialObjectMetadata{}
		obj.SetName("child")
		obj.SetNamespace("default")
		obj.SetLabels(map[string]string{
			metadata.OwnedLabel:             "true",
			metadata.InstanceLabel:          "parent",
			metadata.InstanceNamespaceLabel: "default",
			metadata.InstanceGroupLabel:     "wrong-group",
			metadata.InstanceVersionLabel:   "v1",
			metadata.InstanceKindLabel:      "ConfigMap",
		})

		funcs.AddFunc(obj)
		assert.Equal(t, 0, dc.queue.Len())
	})

	t.Run("correct labels should enqueue", func(t *testing.T) {
		obj := &v1.PartialObjectMetadata{}
		obj.SetName("child")
		obj.SetNamespace("default")
		obj.SetLabels(map[string]string{
			metadata.OwnedLabel:             "true",
			metadata.InstanceLabel:          "parent",
			metadata.InstanceNamespaceLabel: "ns",
			metadata.InstanceGroupLabel:     "",
			metadata.InstanceVersionLabel:   "v1",
			metadata.InstanceKindLabel:      "ConfigMap",
		})

		funcs.AddFunc(obj)
		assert.Equal(t, 1, dc.queue.Len())

		item, _ := dc.queue.Get()
		dc.queue.Done(item)
		dc.queue.Forget(item)
	})

	t.Run("invalid object type", func(t *testing.T) {
		funcs.AddFunc(struct{}{})
		assert.Equal(t, 0, dc.queue.Len())
	})

	t.Run("update and delete events", func(t *testing.T) {
		obj := &v1.PartialObjectMetadata{}
		obj.SetName("child2")
		obj.SetNamespace("default")
		obj.SetLabels(map[string]string{
			metadata.OwnedLabel:             "true",
			metadata.InstanceLabel:          "parent2",
			metadata.InstanceNamespaceLabel: "ns2",
			metadata.InstanceGroupLabel:     "",
			metadata.InstanceVersionLabel:   "v1",
			metadata.InstanceKindLabel:      "ConfigMap",
		})

		funcs.UpdateFunc(nil, obj)
		assert.Equal(t, 1, dc.queue.Len())
		item, _ := dc.queue.Get()
		dc.queue.Done(item)
		dc.queue.Forget(item)

		funcs.DeleteFunc(obj)
		assert.Equal(t, 1, dc.queue.Len())
		item, _ = dc.queue.Get()
		dc.queue.Done(item)
		dc.queue.Forget(item)
	})
}

func TestHandlerForChildGVR_MapperError(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))

	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)

	parentGVR := schema.GroupVersionResource{Group: "unknown", Version: "v1", Resource: "unknowns"}
	childGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}

	_, err := dc.handlerForChildGVR(parentGVR, childGVR)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed")
}

func TestGracefulShutdown_Timeout(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	cfg := testConfig()
	cfg.QueueShutdownTimeout = 10 * time.Millisecond
	dc := NewDynamicController(noopLogger(), cfg, client, mapper)

	blockCh := make(chan struct{})
	dc.queue = &blockingQueue{
		TypedRateLimitingInterface: dc.queue,
		blockCh:                    blockCh,
	}

	err := dc.gracefulShutdown()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "timeout")
	close(blockCh)
}

func TestGracefulShutdown_NoTimeout(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	cfg := testConfig()
	cfg.QueueShutdownTimeout = 0
	dc := NewDynamicController(noopLogger(), cfg, client, mapper)

	err := dc.gracefulShutdown()
	assert.NoError(t, err)
}

func TestDeregister_NotRegistered(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)

	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	err := dc.Deregister(t.Context(), gvr)
	assert.NoError(t, err)
}

func TestRemoveHandlerLocked_NoWatch(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)

	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	err := dc.removeHandlerLocked(gvr, "some-handler-id")
	assert.NoError(t, err)
}

func TestReconcileParentLocked_NoHandlerNoop(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)
	reg := &registration{parentGVR: gvr, childHandlerIDs: make(map[schema.GroupVersionResource]string)}

	dc.mu.Lock()
	err := dc.reconcileParentLocked(gvr, nil, reg)
	dc.mu.Unlock()

	assert.NoError(t, err)
	assert.Empty(t, reg.parentHandlerID)
	assert.Empty(t, dc.watches)
}

func TestReconcileParentLocked_RemoveHandlerError(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}
	handlerID := parentHandlerID(gvr)

	watch := internal.NewLazyInformer(client, gvr, time.Second, nil, noopLogger())
	require.NoError(t, watch.AddHandler(t.Context(), handlerID, cache.ResourceEventHandlerFuncs{}))
	t.Cleanup(watch.Shutdown)

	watch.SetInformerForTesting(&erroringInformer{
		SharedIndexInformer: watch.Informer(),
		removeErr:           fmt.Errorf("remove failed"),
	})

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)
	dc.watches[gvr] = watch
	reg := &registration{
		parentGVR:       gvr,
		parentHandlerID: handlerID,
		childHandlerIDs: make(map[schema.GroupVersionResource]string),
	}

	dc.mu.Lock()
	err := dc.reconcileParentLocked(gvr, nil, reg)
	dc.mu.Unlock()

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "removing parent handler")
}

func TestRemoveHandlerLocked_Error(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	watch := internal.NewLazyInformer(client, gvr, time.Second, nil, noopLogger())
	require.NoError(t, watch.AddHandler(t.Context(), "handler-1", cache.ResourceEventHandlerFuncs{}))

	watch.SetInformerForTesting(&erroringInformer{
		SharedIndexInformer: watch.Informer(),
		removeErr:           fmt.Errorf("remove failed"),
	})

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)
	dc.watches[gvr] = watch

	err := dc.removeHandlerLocked(gvr, "handler-1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "removing handler")

	watch.Shutdown()
}

func TestRegister_WithChildren(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1.AddMetaToScheme(scheme))

	parentGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	parentGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	childGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}

	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())
	mapper.Add(parentGVK, meta.RESTScopeNamespace)

	client := fake.NewSimpleMetadataClient(scheme)

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() {
		_ = dc.Start(ctx)
	}()
	require.Eventually(t, func() bool { return dc.ctx.Load() != nil }, 2*time.Second, 10*time.Millisecond)

	handler := Handler(func(ctx context.Context, req controllerruntime.Request) error {
		return nil
	})

	err := dc.Register(ctx, parentGVR, handler, childGVR)
	require.NoError(t, err)

	dc.mu.Lock()
	reg, exists := dc.registrations[parentGVR]
	dc.mu.Unlock()
	assert.True(t, exists)
	assert.NotEmpty(t, reg.parentHandlerID)
	assert.Len(t, reg.childHandlerIDs, 1)

	err = dc.Register(ctx, parentGVR, handler)
	require.NoError(t, err)

	dc.mu.Lock()
	reg = dc.registrations[parentGVR]
	dc.mu.Unlock()
	assert.Empty(t, reg.childHandlerIDs)

	err = dc.Deregister(ctx, parentGVR)
	require.NoError(t, err)

	dc.mu.Lock()
	_, exists = dc.registrations[parentGVR]
	dc.mu.Unlock()
	assert.False(t, exists)
}

func TestReconcileParentLocked_RemoveHandler(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))

	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() {
		_ = dc.Start(ctx)
	}()
	require.Eventually(t, func() bool { return dc.ctx.Load() != nil }, 2*time.Second, 10*time.Millisecond)

	handler := Handler(func(ctx context.Context, req controllerruntime.Request) error {
		return nil
	})

	err := dc.Register(ctx, gvr, handler)
	require.NoError(t, err)

	dc.mu.Lock()
	reg := dc.registrations[gvr]
	assert.NotEmpty(t, reg.parentHandlerID)
	dc.mu.Unlock()

	err = dc.Deregister(ctx, gvr)
	require.NoError(t, err)

	dc.mu.Lock()
	_, exists := dc.registrations[gvr]
	dc.mu.Unlock()
	assert.False(t, exists)
}

func TestReconcileParentLocked_HandlerFuncs(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	parentGVR := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)
	ctx := t.Context()
	dc.ctx.Store(&ctx)

	watch := internal.NewLazyInformer(client, parentGVR, time.Second, nil, noopLogger())
	capture := &capturingInformer{}
	watch.SetInformerForTesting(capture)
	dc.watches[parentGVR] = watch
	t.Cleanup(watch.Shutdown)

	reg := &registration{parentGVR: parentGVR, childHandlerIDs: make(map[schema.GroupVersionResource]string)}
	handler := Handler(func(ctx context.Context, req controllerruntime.Request) error { return nil })

	dc.mu.Lock()
	err := dc.reconcileParentLocked(parentGVR, handler, reg)
	dc.mu.Unlock()
	require.NoError(t, err)

	funcs, ok := capture.handler.(cache.ResourceEventHandlerFuncs)
	require.True(t, ok)

	oldObj := &v1.PartialObjectMetadata{}
	oldObj.SetName("parent")
	oldObj.SetNamespace("default")
	oldObj.SetGeneration(1)
	newObj := oldObj.DeepCopy()
	newObj.SetGeneration(2)

	funcs.UpdateFunc(oldObj, newObj)
	assert.Equal(t, 1, dc.queue.Len())
	item, _ := dc.queue.Get()
	dc.queue.Done(item)
	dc.queue.Forget(item)

	funcs.DeleteFunc(newObj)
	assert.Equal(t, 1, dc.queue.Len())

	item, _ = dc.queue.Get()
	dc.queue.Done(item)
	dc.queue.Forget(item)
}

func TestReconcileChildrenLocked_RemoveHandlerError(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))

	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	parentGVR := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "parents"}
	childGVR := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "children"}
	handlerID := childHandlerID(parentGVR, childGVR)

	watch := internal.NewLazyInformer(client, childGVR, time.Second, nil, noopLogger())
	require.NoError(t, watch.AddHandler(t.Context(), handlerID, cache.ResourceEventHandlerFuncs{}))
	t.Cleanup(watch.Shutdown)

	watch.SetInformerForTesting(&erroringInformer{
		SharedIndexInformer: watch.Informer(),
		removeErr:           fmt.Errorf("remove failed"),
	})

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)
	dc.watches[childGVR] = watch
	reg := &registration{
		parentGVR:       parentGVR,
		childHandlerIDs: map[schema.GroupVersionResource]string{childGVR: handlerID},
	}

	dc.mu.Lock()
	err := dc.reconcileChildrenLocked(parentGVR, nil, reg)
	dc.mu.Unlock()

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "removing child handler")
}

func TestReconcileChildrenLocked_NoChange(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	parentGVR := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "parents"}
	childGVR := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "children"}

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)
	reg := &registration{
		parentGVR:       parentGVR,
		childHandlerIDs: map[schema.GroupVersionResource]string{childGVR: "child-handler"},
	}

	dc.mu.Lock()
	err := dc.reconcileChildrenLocked(parentGVR, []schema.GroupVersionResource{childGVR}, reg)
	dc.mu.Unlock()

	assert.NoError(t, err)
	assert.Len(t, reg.childHandlerIDs, 1)
}

func TestReconcileChildrenLocked_HandlerForChildError(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	parentGVR := schema.GroupVersionResource{Group: "missing", Version: "v1", Resource: "parents"}
	childGVR := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "children"}

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)
	reg := &registration{parentGVR: parentGVR, childHandlerIDs: make(map[schema.GroupVersionResource]string)}

	dc.mu.Lock()
	err := dc.reconcileChildrenLocked(parentGVR, []schema.GroupVersionResource{childGVR}, reg)
	dc.mu.Unlock()

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "creating child handler")
}

func TestReconcileChildrenLocked_RemoveObsolete(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1.AddMetaToScheme(scheme))

	parentGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	parentGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	child1GVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
	child2GVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	child2GVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}

	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())
	mapper.Add(parentGVK, meta.RESTScopeNamespace)
	mapper.Add(child2GVK, meta.RESTScopeNamespace)

	client := fake.NewSimpleMetadataClient(scheme)

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() {
		_ = dc.Start(ctx)
	}()
	require.Eventually(t, func() bool { return dc.ctx.Load() != nil }, 2*time.Second, 10*time.Millisecond)

	handler := Handler(func(ctx context.Context, req controllerruntime.Request) error {
		return nil
	})

	err := dc.Register(ctx, parentGVR, handler, child1GVR)
	require.NoError(t, err)

	dc.mu.Lock()
	reg := dc.registrations[parentGVR]
	dc.mu.Unlock()
	assert.Len(t, reg.childHandlerIDs, 1)
	_, hasChild1 := reg.childHandlerIDs[child1GVR]
	assert.True(t, hasChild1)

	err = dc.Register(ctx, parentGVR, handler, child2GVR)
	require.NoError(t, err)

	dc.mu.Lock()
	reg = dc.registrations[parentGVR]
	dc.mu.Unlock()
	assert.Len(t, reg.childHandlerIDs, 1)
	_, hasChild1 = reg.childHandlerIDs[child1GVR]
	assert.False(t, hasChild1)
	_, hasChild2 := reg.childHandlerIDs[child2GVR]
	assert.True(t, hasChild2)
}

func TestKeyFromGVR(t *testing.T) {
	tests := []struct {
		name     string
		gvr      schema.GroupVersionResource
		expected string
	}{
		{
			name:     "full gvr",
			gvr:      schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			expected: "apps/v1/deployments",
		},
		{
			name:     "core api (empty group)",
			gvr:      schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			expected: "/v1/pods",
		},
		{
			name:     "empty gvr",
			gvr:      schema.GroupVersionResource{},
			expected: "",
		},
		{
			name:     "only group",
			gvr:      schema.GroupVersionResource{Group: "apps"},
			expected: "apps",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := keyFromGVR(tt.gvr)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestChildAndParentHandlerID(t *testing.T) {
	parent := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	child := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}

	parentID := parentHandlerID(parent)
	assert.Equal(t, "parent:apps/v1/deployments", parentID)

	childID := childHandlerID(parent, child)
	assert.Equal(t, "child:apps/v1/deployments->/v1/pods", childID)
}

func TestRegister_AddHandlerError(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))

	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)

	ctx, cancel := context.WithCancel(t.Context())

	go func() {
		_ = dc.Start(ctx)
	}()
	require.Eventually(t, func() bool { return dc.ctx.Load() != nil }, 2*time.Second, 10*time.Millisecond)

	cancel()
	time.Sleep(50 * time.Millisecond)

	handler := Handler(func(ctx context.Context, req controllerruntime.Request) error {
		return nil
	})

	err := dc.Register(ctx, gvr, handler)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "add parent handler")
}

func TestRegister_AddChildHandlerError(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1.AddMetaToScheme(scheme))

	parentGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	parentGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	childGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}

	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())
	mapper.Add(parentGVK, meta.RESTScopeNamespace)

	client := fake.NewSimpleMetadataClient(scheme)

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)

	ctx, cancel := context.WithCancel(t.Context())

	go func() {
		_ = dc.Start(ctx)
	}()
	require.Eventually(t, func() bool { return dc.ctx.Load() != nil }, 2*time.Second, 10*time.Millisecond)

	handler := Handler(func(ctx context.Context, req controllerruntime.Request) error {
		return nil
	})

	err := dc.Register(ctx, parentGVR, handler)
	require.NoError(t, err)

	cancel()
	time.Sleep(50 * time.Millisecond)

	err = dc.Register(ctx, parentGVR, handler, childGVR)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "add child handler")
}

func TestDeregister_DetachErrors(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1.AddMetaToScheme(scheme))

	parentGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	parentGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	childGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}

	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())
	mapper.Add(parentGVK, meta.RESTScopeNamespace)

	client := fake.NewSimpleMetadataClient(scheme)

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() {
		_ = dc.Start(ctx)
	}()
	require.Eventually(t, func() bool { return dc.ctx.Load() != nil }, 2*time.Second, 10*time.Millisecond)

	handler := Handler(func(ctx context.Context, req controllerruntime.Request) error {
		return nil
	})

	err := dc.Register(ctx, parentGVR, handler, childGVR)
	require.NoError(t, err)

	dc.mu.Lock()
	if w, ok := dc.watches[childGVR]; ok {
		w.Shutdown()
	}
	if w, ok := dc.watches[parentGVR]; ok {
		w.Shutdown()
	}
	dc.mu.Unlock()

	err = dc.Deregister(ctx, parentGVR)
	assert.NoError(t, err)
}

func TestDeregister_ReconcileErrors(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	parentGVR := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "parents"}
	childGVR := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "children"}
	parentID := parentHandlerID(parentGVR)
	childID := childHandlerID(parentGVR, childGVR)

	parentWatch := internal.NewLazyInformer(client, parentGVR, time.Second, nil, noopLogger())
	require.NoError(t, parentWatch.AddHandler(t.Context(), parentID, cache.ResourceEventHandlerFuncs{}))
	t.Cleanup(parentWatch.Shutdown)
	parentWatch.SetInformerForTesting(&erroringInformer{
		SharedIndexInformer: parentWatch.Informer(),
		removeErr:           fmt.Errorf("remove failed"),
	})

	childWatch := internal.NewLazyInformer(client, childGVR, time.Second, nil, noopLogger())
	require.NoError(t, childWatch.AddHandler(t.Context(), childID, cache.ResourceEventHandlerFuncs{}))
	t.Cleanup(childWatch.Shutdown)
	childWatch.SetInformerForTesting(&erroringInformer{
		SharedIndexInformer: childWatch.Informer(),
		removeErr:           fmt.Errorf("remove failed"),
	})

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)
	dc.watches[parentGVR] = parentWatch
	dc.watches[childGVR] = childWatch
	dc.registrations[parentGVR] = &registration{
		parentGVR:       parentGVR,
		parentHandlerID: parentID,
		childHandlerIDs: map[schema.GroupVersionResource]string{childGVR: childID},
	}

	err := dc.Deregister(t.Context(), parentGVR)
	assert.NoError(t, err)

	dc.mu.Lock()
	_, exists := dc.registrations[parentGVR]
	dc.mu.Unlock()
	assert.False(t, exists)
}
