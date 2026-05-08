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
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/workqueue"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
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

	_, exists := dc.parentWatches.Load(gvr)
	assert.True(t, exists)

	// Try to register again (should not fail)
	err = dc.Register(t.Context(), gvr, handlerFunc)
	assert.NoError(t, err)

	// Unregister GVK
	shutdownContext, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err = dc.Deregister(shutdownContext, gvr)
	require.NoError(t, err)

	_, exists = dc.parentWatches.Load(gvr)
	assert.False(t, exists)

	// Parent informer should be stopped after deregister.
	assert.Nil(t, dc.watches.GetInformer(gvr), "parent informer should stop after deregister")
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

	parentGVR := schema.GroupVersionResource{Group: "group", Version: "version", Resource: "resource"}

	dc.enqueueParent(parentGVR, Event{
		Type:      EventAdd,
		GVR:       parentGVR,
		Name:      "test-object",
		Namespace: "default",
	})

	assert.Equal(t, 1, dc.queue.Len())
}

func TestChildCleanup_DoesNotStopParentInformer(t *testing.T) {
	logger := noopLogger()
	client, mapper := setupFakeClient(t)

	dc := NewDynamicController(logger, testConfig(), client, mapper)
	ctx := t.Context()
	dc.ctx.Store(&ctx)

	parentGVR := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}
	consumerParentGVR := schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "consumers"}

	handlerFunc := Handler(func(ctx context.Context, req controllerruntime.Request) error {
		return nil
	})

	require.NoError(t, dc.Register(ctx, parentGVR, handlerFunc))
	assert.NotNil(t, dc.watches.GetInformer(parentGVR), "parent informer should be running after register")

	instance := types.NamespacedName{Name: "consumer", Namespace: "default"}
	watcher := dc.coordinator.ForInstance(consumerParentGVR, instance)
	require.NoError(t, watcher.Watch(WatchRequest{
		NodeID:    "external",
		GVR:       parentGVR,
		Name:      "target",
		Namespace: "default",
	}))
	watcher.Done(true)

	dc.coordinator.RemoveInstance(consumerParentGVR, instance)
	assert.NotNil(t, dc.watches.GetInformer(parentGVR), "child cleanup must not stop a registered parent informer")

	require.NoError(t, dc.Deregister(ctx, parentGVR))
	assert.Nil(t, dc.watches.GetInformer(parentGVR), "informer should stop once both parent and child users are gone")
}

func TestDeregister_KeepsInformerWhileChildWatchRemains(t *testing.T) {
	logger := noopLogger()
	client, mapper := setupFakeClient(t)

	dc := NewDynamicController(logger, testConfig(), client, mapper)
	ctx := t.Context()
	dc.ctx.Store(&ctx)

	parentGVR := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}
	consumerParentGVR := schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "consumers"}

	handlerFunc := Handler(func(ctx context.Context, req controllerruntime.Request) error {
		return nil
	})

	require.NoError(t, dc.Register(ctx, parentGVR, handlerFunc))

	instance := types.NamespacedName{Name: "consumer", Namespace: "default"}
	watcher := dc.coordinator.ForInstance(consumerParentGVR, instance)
	require.NoError(t, watcher.Watch(WatchRequest{
		NodeID:    "external",
		GVR:       parentGVR,
		Name:      "target",
		Namespace: "default",
	}))
	watcher.Done(true)

	require.NoError(t, dc.Deregister(ctx, parentGVR))
	assert.NotNil(t, dc.watches.GetInformer(parentGVR), "child watch should keep informer alive after parent deregister")

	dc.coordinator.RemoveInstance(consumerParentGVR, instance)
	assert.Nil(t, dc.watches.GetInformer(parentGVR), "informer should stop after the remaining child watch is removed")
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
	dc.ctx.Store(new(t.Context())) // simulate a start through dc.Run

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

func TestEnqueueFromInformer_GenerationSkip(t *testing.T) {
	parentGVR := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	makeObj := func(name, ns string, gen int64, annotations map[string]string) *v1.PartialObjectMetadata {
		obj := &v1.PartialObjectMetadata{}
		obj.SetName(name)
		obj.SetNamespace(ns)
		obj.SetGeneration(gen)
		obj.SetAnnotations(annotations)
		return obj
	}

	tests := []struct {
		name        string
		oldObj      *v1.PartialObjectMetadata
		newObj      *v1.PartialObjectMetadata
		expectQueue int
	}{
		{
			name:        "generation changed — enqueues",
			oldObj:      makeObj("a", "default", 1, nil),
			newObj:      makeObj("a", "default", 2, nil),
			expectQueue: 1,
		},
		{
			name:        "same generation — skips",
			oldObj:      makeObj("a", "default", 5, nil),
			newObj:      makeObj("a", "default", 5, nil),
			expectQueue: 0,
		},
		{
			name:        "same generation but reconcile re-enabled — enqueues",
			oldObj:      makeObj("a", "default", 5, map[string]string{v1alpha1.InstanceReconcileAnnotation: "disabled"}),
			newObj:      makeObj("a", "default", 5, nil),
			expectQueue: 1,
		},
		{
			name:        "same generation, reconcile stays disabled — skips",
			oldObj:      makeObj("a", "default", 5, map[string]string{v1alpha1.InstanceReconcileAnnotation: "disabled"}),
			newObj:      makeObj("a", "default", 5, map[string]string{v1alpha1.InstanceReconcileAnnotation: "disabled"}),
			expectQueue: 0,
		},
		{
			name:        "same generation, reconcile disabled on new only — skips",
			oldObj:      makeObj("a", "default", 5, nil),
			newObj:      makeObj("a", "default", 5, map[string]string{v1alpha1.InstanceReconcileAnnotation: "disabled"}),
			expectQueue: 0,
		},
		{
			name:        "same generation, reconcile re-enabled case-insensitive — enqueues",
			oldObj:      makeObj("a", "default", 5, map[string]string{v1alpha1.InstanceReconcileAnnotation: "Disabled"}),
			newObj:      makeObj("a", "default", 5, map[string]string{v1alpha1.InstanceReconcileAnnotation: "true"}),
			expectQueue: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, v1.AddMetaToScheme(scheme))
			client := fake.NewSimpleMetadataClient(scheme)
			mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

			dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)
			dc.enqueueFromInformer(parentGVR, tt.oldObj, tt.newObj, EventUpdate)
			assert.Equal(t, tt.expectQueue, dc.queue.Len())
		})
	}
}

func TestEnqueueFromInformer_AddAndDelete(t *testing.T) {
	// Add and Delete pass nil for oldObject — generation skip is bypassed,
	// so these events should always enqueue.
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	parentGVR := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}
	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)

	obj := &v1.PartialObjectMetadata{}
	obj.SetName("a")
	obj.SetNamespace("default")
	obj.SetGeneration(1)

	// AddFunc passes nil as oldObject — should enqueue
	dc.enqueueFromInformer(parentGVR, nil, obj, EventAdd)
	assert.Equal(t, 1, dc.queue.Len(), "Add events should be enqueued")

	// Drain
	item, _ := dc.queue.Get()
	dc.queue.Done(item)
	dc.queue.Forget(item)

	// DeleteFunc passes nil as oldObject — should enqueue
	dc.enqueueFromInformer(parentGVR, nil, obj, EventDelete)
	assert.Equal(t, 1, dc.queue.Len(), "Delete events should be enqueued")
}

func TestEnqueueFromInformer_NilNewObject(t *testing.T) {
	// If newObject is nil, meta.Accessor fails and we return early.
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	parentGVR := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}
	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)

	dc.enqueueFromInformer(parentGVR, nil, nil, EventDelete)
	assert.Equal(t, 0, dc.queue.Len(), "nil newObject should cause early return")
}

func TestReconcileEnabledInUpdate(t *testing.T) {
	makeObj := func(annotations map[string]string) *v1.PartialObjectMetadata {
		obj := &v1.PartialObjectMetadata{}
		obj.SetAnnotations(annotations)
		return obj
	}

	tests := []struct {
		name   string
		old    map[string]string
		new    map[string]string
		expect bool
	}{
		// Legacy "disabled" value (backward compat)
		{
			name:   "disabled -> enabled (annotation removed)",
			old:    map[string]string{v1alpha1.InstanceReconcileAnnotation: "disabled"},
			new:    nil,
			expect: true,
		},
		{
			name:   "disabled -> enabled (annotation changed)",
			old:    map[string]string{v1alpha1.InstanceReconcileAnnotation: "disabled"},
			new:    map[string]string{v1alpha1.InstanceReconcileAnnotation: "enabled"},
			expect: true,
		},
		{
			name:   "disabled -> disabled",
			old:    map[string]string{v1alpha1.InstanceReconcileAnnotation: "disabled"},
			new:    map[string]string{v1alpha1.InstanceReconcileAnnotation: "disabled"},
			expect: false,
		},
		{
			name:   "enabled -> disabled",
			old:    nil,
			new:    map[string]string{v1alpha1.InstanceReconcileAnnotation: "disabled"},
			expect: false,
		},
		{
			name:   "case insensitive disabled",
			old:    map[string]string{v1alpha1.InstanceReconcileAnnotation: "DISABLED"},
			new:    map[string]string{v1alpha1.InstanceReconcileAnnotation: "true"},
			expect: true,
		},
		// Canonical "suspended" value
		{
			name:   "suspended -> enabled (annotation removed)",
			old:    map[string]string{v1alpha1.InstanceReconcileAnnotation: "suspended"},
			new:    nil,
			expect: true,
		},
		{
			name:   "suspended -> enabled (annotation changed)",
			old:    map[string]string{v1alpha1.InstanceReconcileAnnotation: "suspended"},
			new:    map[string]string{v1alpha1.InstanceReconcileAnnotation: "enabled"},
			expect: true,
		},
		{
			name:   "suspended -> suspended",
			old:    map[string]string{v1alpha1.InstanceReconcileAnnotation: "suspended"},
			new:    map[string]string{v1alpha1.InstanceReconcileAnnotation: "suspended"},
			expect: false,
		},
		{
			name:   "enabled -> suspended",
			old:    nil,
			new:    map[string]string{v1alpha1.InstanceReconcileAnnotation: "suspended"},
			expect: false,
		},
		{
			name:   "case insensitive suspended",
			old:    map[string]string{v1alpha1.InstanceReconcileAnnotation: "SUSPENDED"},
			new:    map[string]string{v1alpha1.InstanceReconcileAnnotation: "true"},
			expect: true,
		},
		// Cross-value transitions
		{
			name:   "disabled -> suspended (both suspend, no re-enqueue)",
			old:    map[string]string{v1alpha1.InstanceReconcileAnnotation: "disabled"},
			new:    map[string]string{v1alpha1.InstanceReconcileAnnotation: "suspended"},
			expect: false,
		},
		{
			name:   "suspended -> disabled (both suspend, no re-enqueue)",
			old:    map[string]string{v1alpha1.InstanceReconcileAnnotation: "suspended"},
			new:    map[string]string{v1alpha1.InstanceReconcileAnnotation: "disabled"},
			expect: false,
		},
		{
			name:   "no annotations on either",
			old:    nil,
			new:    nil,
			expect: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := reconcileEnabledInUpdate(makeObj(tt.old), makeObj(tt.new))
			assert.Equal(t, tt.expect, result)
		})
	}
}

func TestMetadataNeedsReconcile(t *testing.T) {
	makeObj := func(labels, annotations map[string]string) *v1.PartialObjectMetadata {
		obj := &v1.PartialObjectMetadata{}
		obj.SetLabels(labels)
		obj.SetAnnotations(annotations)
		return obj
	}
	suspended := map[string]string{v1alpha1.InstanceReconcileAnnotation: "disabled"}

	tests := []struct {
		name   string
		old    *v1.PartialObjectMetadata
		new    *v1.PartialObjectMetadata
		expect bool
	}{
		{
			name:   "both nil — no change",
			old:    makeObj(nil, nil),
			new:    makeObj(nil, nil),
			expect: false,
		},
		{
			name:   "same labels — no change",
			old:    makeObj(map[string]string{"team": "platform"}, nil),
			new:    makeObj(map[string]string{"team": "platform"}, nil),
			expect: false,
		},
		{
			name:   "same annotations — no change",
			old:    makeObj(nil, map[string]string{"env": "prod"}),
			new:    makeObj(nil, map[string]string{"env": "prod"}),
			expect: false,
		},
		{
			name:   "label added",
			old:    makeObj(nil, nil),
			new:    makeObj(map[string]string{"team": "platform"}, nil),
			expect: true,
		},
		{
			name:   "label removed",
			old:    makeObj(map[string]string{"team": "platform"}, nil),
			new:    makeObj(nil, nil),
			expect: true,
		},
		{
			name:   "label value changed",
			old:    makeObj(map[string]string{"team": "platform"}, nil),
			new:    makeObj(map[string]string{"team": "infra"}, nil),
			expect: true,
		},
		{
			name:   "annotation added",
			old:    makeObj(nil, nil),
			new:    makeObj(nil, map[string]string{"env": "prod"}),
			expect: true,
		},
		{
			name:   "annotation removed",
			old:    makeObj(nil, map[string]string{"env": "prod"}),
			new:    makeObj(nil, nil),
			expect: true,
		},
		{
			name:   "annotation value changed",
			old:    makeObj(nil, map[string]string{"env": "prod"}),
			new:    makeObj(nil, map[string]string{"env": "staging"}),
			expect: true,
		},
		{
			name:   "both labels and annotations changed",
			old:    makeObj(map[string]string{"k": "a"}, map[string]string{"env": "prod"}),
			new:    makeObj(map[string]string{"k": "b"}, map[string]string{"env": "staging"}),
			expect: true,
		},
		{
			name:   "reconcile suspended on new — label change ignored",
			old:    makeObj(map[string]string{"team": "platform"}, nil),
			new:    makeObj(map[string]string{"team": "infra"}, suspended),
			expect: false,
		},
		{
			name:   "reconcile suspended on new — annotation change ignored",
			old:    makeObj(nil, map[string]string{"env": "prod"}),
			new:    makeObj(nil, suspended),
			expect: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expect, metadataNeedsReconcile(tt.old, tt.new))
		})
	}
}

func TestEnqueueFromInformer_MetadataChange(t *testing.T) {
	parentGVR := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	makeObj := func(name, ns string, gen int64, labels, annotations map[string]string) *v1.PartialObjectMetadata {
		obj := &v1.PartialObjectMetadata{}
		obj.SetName(name)
		obj.SetNamespace(ns)
		obj.SetGeneration(gen)
		obj.SetLabels(labels)
		obj.SetAnnotations(annotations)
		return obj
	}

	tests := []struct {
		name        string
		oldObj      *v1.PartialObjectMetadata
		newObj      *v1.PartialObjectMetadata
		expectQueue int
	}{
		{
			name:        "same generation, label added — enqueues for metadata propagation",
			oldObj:      makeObj("a", "default", 5, nil, nil),
			newObj:      makeObj("a", "default", 5, map[string]string{"team": "platform"}, nil),
			expectQueue: 1,
		},
		{
			name:        "same generation, label removed — enqueues for metadata propagation",
			oldObj:      makeObj("a", "default", 5, map[string]string{"team": "platform"}, nil),
			newObj:      makeObj("a", "default", 5, nil, nil),
			expectQueue: 1,
		},
		{
			name:        "same generation, annotation changed — enqueues for metadata propagation",
			oldObj:      makeObj("a", "default", 5, nil, map[string]string{"env": "prod"}),
			newObj:      makeObj("a", "default", 5, nil, map[string]string{"env": "staging"}),
			expectQueue: 1,
		},
		{
			name:        "same generation, identical metadata — skips",
			oldObj:      makeObj("a", "default", 5, map[string]string{"team": "platform"}, nil),
			newObj:      makeObj("a", "default", 5, map[string]string{"team": "platform"}, nil),
			expectQueue: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, v1.AddMetaToScheme(scheme))
			client := fake.NewSimpleMetadataClient(scheme)
			mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

			dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)
			dc.enqueueFromInformer(parentGVR, tt.oldObj, tt.newObj, EventUpdate)
			assert.Equal(t, tt.expectQueue, dc.queue.Len())
		})
	}
}

func TestRegister_EnsureWatchSyncError(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))

	client := fake.NewSimpleMetadataClient(scheme)
	// Fail all list calls so the informer cannot sync.
	client.PrependReactor("list", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("simulated list error")
	})
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)
	// Short sync timeout so test doesn't wait 30s.
	dc.watches.SyncTimeout = 200 * time.Millisecond

	ctx := t.Context()
	go func() {
		_ = dc.Start(ctx)
	}()
	require.Eventually(t, func() bool { return dc.ctx.Load() != nil }, 2*time.Second, 10*time.Millisecond)

	handler := Handler(func(ctx context.Context, req controllerruntime.Request) error {
		return nil
	})

	err := dc.Register(ctx, gvr, handler)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cache sync timeout")
}

func TestGetInformer_ReturnsNil_ForMissingWatch(t *testing.T) {
	// Verify that GetInformer returns nil when no watch exists, and
	// non-nil when a watch is active. This is the condition the nil
	// guard in Register protects against.
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	wm := NewWatchManager(fake.NewSimpleMetadataClient(scheme), 1*time.Hour, func(Event) {}, noopLogger())

	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}
	assert.Nil(t, wm.GetInformer(gvr), "GetInformer should return nil for unwatched GVR")

	require.NoError(t, wm.EnsureWatch(gvr, "owner"))
	assert.NotNil(t, wm.GetInformer(gvr))

	wm.forceStopWatch(gvr)
	assert.Nil(t, wm.GetInformer(gvr), "GetInformer should return nil after forceStopWatch")
}

func TestDeregister_HandlerNoLongerReceivesEvents(t *testing.T) {
	logger := noopLogger()
	client, mapper := setupFakeClient(t)

	dc := NewDynamicController(logger, testConfig(), client, mapper)
	ctx := t.Context()
	dc.ctx.Store(&ctx)

	parentGVR := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	handlerCalled := false
	handlerFunc := Handler(func(ctx context.Context, req controllerruntime.Request) error {
		handlerCalled = true
		return nil
	})

	require.NoError(t, dc.Register(ctx, parentGVR, handlerFunc))
	require.NoError(t, dc.Deregister(ctx, parentGVR))

	// After deregister, handler lookup should fail.
	_, ok := dc.handlers.Load(parentGVR)
	assert.False(t, ok, "handler should be removed after deregister")
	assert.False(t, handlerCalled)
}
