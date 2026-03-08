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

	_, exists := dc.parentWatches[gvr]
	assert.True(t, exists)

	// Try to register again (should not fail)
	err = dc.Register(t.Context(), gvr, handlerFunc)
	assert.NoError(t, err)

	// Unregister GVK
	shutdownContext, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err = dc.Deregister(shutdownContext, gvr)
	require.NoError(t, err)

	_, exists = dc.parentWatches[gvr]
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

// TestChildCleanup_DoesNotStopParentInformer verifies that when a child/externalRef
// watch shares the same GVR as a parent watch, cleaning up the child's coordinator
// reference does not stop the parent's informer. This is the core bug that keyed
// ownership prevents: without separate ownership keys, releasing the coordinator's
// reference would kill the parent informer.
func TestChildCleanup_DoesNotStopParentInformer(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	// Producer RGD's parent GVR — also used as an externalRef by the consumer.
	producerGVR := schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "producers"}
	// Consumer RGD's parent GVR.
	consumerGVR := schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "consumers"}

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)
	ctx := t.Context()
	go func() { _ = dc.Start(ctx) }()
	require.Eventually(t, func() bool { return dc.ctx.Load() != nil }, 2*time.Second, 10*time.Millisecond)

	noopHandler := Handler(func(_ context.Context, _ controllerruntime.Request) error { return nil })

	// Register producer as a parent — acquires informer with key "parent".
	require.NoError(t, dc.Register(ctx, producerGVR, noopHandler))
	assert.NotNil(t, dc.watches.GetInformer(producerGVR), "producer informer should be running")

	// Register consumer as a parent.
	require.NoError(t, dc.Register(ctx, consumerGVR, noopHandler))

	// Simulate consumer's instance reconciler watching producerGVR as an externalRef.
	// This goes through the coordinator, which acquires with key "coordinator".
	consumerInstance := types.NamespacedName{Name: "my-consumer", Namespace: "default"}
	watcher := dc.coordinator.ForInstance(consumerGVR, consumerInstance)
	require.NoError(t, watcher.Watch(WatchRequest{
		NodeID:    "producer-ref",
		GVR:       producerGVR,
		Name:      "my-producer",
		Namespace: "default",
	}))
	watcher.Done()

	// producerGVR now has two owners: "parent" and "coordinator".
	assert.NotNil(t, dc.watches.GetInformer(producerGVR), "producer informer should still be running with two owners")

	// Consumer instance is deleted — coordinator releases its reference.
	dc.coordinator.RemoveInstance(consumerGVR, consumerInstance)

	// The producer's parent informer MUST survive because the "parent" key still owns it.
	assert.NotNil(t, dc.watches.GetInformer(producerGVR),
		"producer informer must survive child cleanup — parent key still holds ownership")
	// 2 active watches: producerGVR (parent) + consumerGVR (parent).
	assert.Equal(t, 2, dc.watches.ActiveWatchCount(),
		"expected both parent informers still active")
}

// TestDeregister_KeepsInformerWhileChildWatchRemains verifies the reverse scenario:
// when a parent is deregistered but the coordinator still holds a reference to the
// same GVR (because another RGD's instance watches it as an externalRef), the
// informer stays alive until the coordinator also releases.
func TestDeregister_KeepsInformerWhileChildWatchRemains(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

	// Shared GVR: acts as both a parent for producerRGD and an externalRef for consumerRGD.
	sharedGVR := schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "shared"}
	consumerGVR := schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "consumers"}

	dc := NewDynamicController(noopLogger(), testConfig(), client, mapper)
	ctx := t.Context()
	go func() { _ = dc.Start(ctx) }()
	require.Eventually(t, func() bool { return dc.ctx.Load() != nil }, 2*time.Second, 10*time.Millisecond)

	noopHandler := Handler(func(_ context.Context, _ controllerruntime.Request) error { return nil })

	// Register sharedGVR as a parent.
	require.NoError(t, dc.Register(ctx, sharedGVR, noopHandler))

	// Register consumer and have an instance watch sharedGVR via coordinator.
	require.NoError(t, dc.Register(ctx, consumerGVR, noopHandler))
	consumerInstance := types.NamespacedName{Name: "my-consumer", Namespace: "default"}
	watcher := dc.coordinator.ForInstance(consumerGVR, consumerInstance)
	require.NoError(t, watcher.Watch(WatchRequest{
		NodeID:    "shared-ref",
		GVR:       sharedGVR,
		Name:      "my-shared",
		Namespace: "default",
	}))
	watcher.Done()

	// Deregister sharedGVR as parent — releases "parent" key.
	require.NoError(t, dc.Deregister(ctx, sharedGVR))

	// Informer MUST survive because coordinator still holds "coordinator" key.
	assert.NotNil(t, dc.watches.GetInformer(sharedGVR),
		"informer must survive parent deregister — coordinator still holds ownership")

	// Now clean up the coordinator reference.
	dc.coordinator.RemoveInstance(consumerGVR, consumerInstance)

	// Now the informer should stop — no owners remain.
	assert.Nil(t, dc.watches.GetInformer(sharedGVR),
		"informer should stop after all owners release")
}

func TestRegister_AcquireSyncError(t *testing.T) {
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
