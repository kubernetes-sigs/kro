// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.
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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/dynamic/fake"
	controllerruntime "sigs.k8s.io/controller-runtime"
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

func setupFakeClient() *fake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}
	gvk := schema.GroupVersionKind{Group: "test", Version: "v1", Kind: "Test"}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	return fake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		gvr: "TestList",
	}, obj)
}

func TestNewDynamicController(t *testing.T) {
	logger := noopLogger()
	client := setupFakeClient()

	config := Config{
		Workers:         2,
		ResyncPeriod:    10 * time.Hour,
		QueueMaxRetries: 20,
		ShutdownTimeout: 60 * time.Second,
	}

	dc := NewDynamicController(logger, config, client)

	assert.NotNil(t, dc)
	assert.Equal(t, config, dc.config)
	assert.NotNil(t, dc.queue)
	assert.NotNil(t, dc.kubeClient)
}

func TestRegisterAndUnregisterGVK(t *testing.T) {
	logger := noopLogger()
	client := setupFakeClient()
	config := Config{
		Workers:         1,
		ResyncPeriod:    1 * time.Second,
		QueueMaxRetries: 5,
		ShutdownTimeout: 5 * time.Second,
	}
	dc := NewDynamicController(logger, config, client)

	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	// Create a context with cancel for running the controller
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the controller in a goroutine
	go func() {
		err := dc.Run(ctx)
		require.NoError(t, err)
	}()

	// Give the controller time to start
	time.Sleep(1 * time.Second)

	handlerFunc := Handler(func(ctx context.Context, req controllerruntime.Request) error {
		return nil
	})

	// Register GVK
	err := dc.StartServingGVK(context.Background(), gvr, handlerFunc)
	require.NoError(t, err)

	_, exists := dc.informers.Load(gvr)
	assert.True(t, exists)

	// Try to register again (should not fail)
	err = dc.StartServingGVK(context.Background(), gvr, handlerFunc)
	assert.NoError(t, err)

	// Unregister GVK
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = dc.StopServiceGVK(shutdownContext, gvr)
	require.NoError(t, err)

	_, exists = dc.informers.Load(gvr)
	assert.False(t, exists)
}

func TestEnqueueObjectErrors(t *testing.T) {
	tests := []struct {
		name       string
		obj        interface{}
		queueItems int
	}{
		{
			name:       "valid unstructured object",
			obj:        newTestObject(),
			queueItems: 1,
		},
		{
			name:       "nil object",
			obj:        nil,
			queueItems: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dc := NewDynamicController(noopLogger(), Config{}, setupFakeClient())

			dc.enqueueObject(tt.obj, "add")

			assert.Equal(t, tt.queueItems, dc.queue.Len())
		})
	}
}

func newTestObject(opts ...TestObjectOption) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}

	obj.SetName("test")
	obj.SetNamespace("default")
	obj.SetGeneration(1)
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "test",
		Version: "v1",
		Kind:    "Test",
	})

	for _, opt := range opts {
		opt(obj)
	}

	return obj
}

type TestObjectOption func(*unstructured.Unstructured)

func TestAllInformerHaveSynced(t *testing.T) {
	tests := []struct {
		name           string
		setupInformers func(*DynamicController)
		expected       bool
	}{
		{
			name:           "no informes",
			setupInformers: func(dc *DynamicController) {},
			expected:       true,
		},
		{
			name: "one unsynced informer",
			setupInformers: func(dc *DynamicController) {
				mockInformer := &mockSharedIndexInformer{hasSynced: false}
				gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}
				dc.informers.Store(gvr, mockInformer)
			},
			expected: false,
		},
		{
			name: "invalid informer type",
			setupInformers: func(dc *DynamicController) {
				gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}
				dc.informers.Store(gvr, "not an informer")
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dc := NewDynamicController(noopLogger(), Config{}, setupFakeClient())
			tt.setupInformers(dc)
			result := dc.AllInformerHaveSynced()
			assert.Equal(t, tt.expected, result)
		})
	}
}

type mockSharedIndexInformer struct {
	hasSynced bool
}

func (m *mockSharedIndexInformer) HasSynced() bool {
	return m.hasSynced
}

func TestUpdateFunc(t *testing.T) {
	tests := []struct {
		name        string
		oldObj      *unstructured.Unstructured
		newObj      *unstructured.Unstructured
		queueLength int
	}{
		{
			name:   "generation changed",
			oldObj: newTestObject(),
			newObj: newTestObject(func(obj *unstructured.Unstructured) {
				obj.SetGeneration(2)
			}),
			queueLength: 1,
		},
		{
			name:        "generation unchanged",
			oldObj:      newTestObject(),
			newObj:      newTestObject(),
			queueLength: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dc := NewDynamicController(noopLogger(), Config{}, setupFakeClient())

			dc.updateFunc(tt.oldObj, tt.newObj)

			assert.Equal(t, tt.queueLength, dc.queue.Len())
		})
	}
}

func TestSyncFunc(t *testing.T) {
	tests := []struct {
		name          string
		setupHandlers func(*DynamicController)
		object        ObjectIdentifiers
		expectError   bool
	}{
		{
			name: "handler exists and succeeds",
			setupHandlers: func(dc *DynamicController) {
				gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}
				dc.handlers.Store(gvr, Handler(func(ctx context.Context, req controllerruntime.Request) error {
					return nil
				}))
			},
			object: ObjectIdentifiers{
				NamespacedKey: "default/test",
				GVR:           schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"},
			},
			expectError: false,
		},
		{
			name: "handler exists but returns error",
			setupHandlers: func(dc *DynamicController) {
				gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}
				dc.handlers.Store(gvr, Handler(func(ctx context.Context, req controllerruntime.Request) error {
					return fmt.Errorf("handler error")
				}))
			},
			object: ObjectIdentifiers{
				NamespacedKey: "default/test",
				GVR:           schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"},
			},
			expectError: true,
		},
		{
			name:          "handler doesn't exist",
			setupHandlers: func(dc *DynamicController) {},
			object: ObjectIdentifiers{
				NamespacedKey: "default/test",
				GVR:           schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"},
			},
			expectError: true,
		},
		{
			name: "invalid handler type",
			setupHandlers: func(dc *DynamicController) {
				gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}
				dc.handlers.Store(gvr, "not a handler") // Wrong type
			},
			object: ObjectIdentifiers{
				NamespacedKey: "default/test",
				GVR:           schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"},
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dc := NewDynamicController(noopLogger(), Config{}, setupFakeClient())
			tt.setupHandlers(dc)

			err := dc.syncFunc(context.Background(), tt.object)

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestGracefulShutdown(t *testing.T) {
	tests := []struct {
		name           string
		setupInformers func(*DynamicController)
		timeout        time.Duration
		expectError    bool
	}{
		{
			name:           "successful shutdown with no informers",
			setupInformers: func(dc *DynamicController) {},
			timeout:        1 * time.Second,
			expectError:    false,
		},
		{
			name: "successful shutdown with informers",
			setupInformers: func(dc *DynamicController) {
				gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}
				dc.informers.Store(gvr, &informerWrapper{
					informer: dynamicinformer.NewDynamicSharedInformerFactory(
						setupFakeClient(), 5*time.Minute),
					shutdown: func() {},
				})
			},
			timeout:     1 * time.Second,
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dc := NewDynamicController(noopLogger(), Config{}, setupFakeClient())
			tt.setupInformers(dc)

			err := dc.gracefulShutdown(tt.timeout)

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
