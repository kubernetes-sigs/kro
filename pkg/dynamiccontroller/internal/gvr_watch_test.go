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

package internal

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/kubernetes-sigs/kro/pkg/dynamiccontroller/watchtracker"
)

func noopLogger() logr.Logger {
	opts := zap.Options{DestWriter: io.Discard}
	return zap.New(zap.UseFlagOptions(&opts))
}

func TestLazyInformer_AddAndRemoveHandler(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	logger := noopLogger()
	li := NewLazyInformer(client, gvr, time.Second, nil, logger, nil)

	// Adding first handler should start informer
	err := li.AddHandler(ctx, "h1", cache.ResourceEventHandlerFuncs{})
	require.NoError(t, err)

	assert.NotNil(t, li.Informer())
	assert.Len(t, li.handlers, 1)

	// Removing it should stop informer
	stopped, err := li.RemoveHandler("h1")
	require.NoError(t, err)
	assert.True(t, stopped)
	assert.Nil(t, li.Informer())
	assert.Empty(t, li.handlers)
}

func TestLazyInformer_MultipleHandlers(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	gvr := schema.GroupVersionResource{Group: "group", Version: "v1", Resource: "resources"}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	logger := noopLogger()
	li := NewLazyInformer(client, gvr, time.Second, nil, logger, nil)

	require.NoError(t, li.AddHandler(ctx, "a", cache.ResourceEventHandlerFuncs{}))
	require.NoError(t, li.AddHandler(ctx, "b", cache.ResourceEventHandlerFuncs{}))
	assert.Len(t, li.handlers, 2)

	// Remove one, informer should remain
	stopped, err := li.RemoveHandler("a")
	require.NoError(t, err)
	assert.False(t, stopped)
	assert.Len(t, li.handlers, 1)
	assert.NotNil(t, li.Informer())

	// Remove second, informer should stop
	stopped, err = li.RemoveHandler("b")
	require.NoError(t, err)
	assert.True(t, stopped)
	assert.Nil(t, li.Informer())
	assert.Empty(t, li.handlers)
}

func TestLazyInformer_Shutdown(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	logger := noopLogger()
	li := NewLazyInformer(client, gvr, time.Second, nil, logger, nil)

	require.NoError(t, li.AddHandler(ctx, "h", cache.ResourceEventHandlerFuncs{}))
	assert.NotNil(t, li.Informer())
	assert.Len(t, li.handlers, 1)

	li.Shutdown()
	assert.Nil(t, li.Informer())
	assert.Empty(t, li.handlers)
}

func TestLazyInformer_RecreateAfterStop(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	ctx := t.Context()
	logger := noopLogger()
	li := NewLazyInformer(client, gvr, time.Second, nil, logger, nil) // avoid tiny resync

	// Add first handler and remove it -> stops informer
	require.NoError(t, li.AddHandler(ctx, "h1", cache.ResourceEventHandlerFuncs{}))
	stopped, err := li.RemoveHandler("h1")
	require.NoError(t, err)
	require.True(t, stopped)
	assert.Nil(t, li.Informer())

	// second removal should be a noop
	stopped, err = li.RemoveHandler("h1")
	assert.NoError(t, err)
	assert.False(t, stopped)

	// Add again — recreate context and informer
	// ignore internal sync failure, just check that informer and handler exist
	_ = li.AddHandler(ctx, "h2", cache.ResourceEventHandlerFuncs{})
	if li.Informer() == nil {
		t.Fatalf("informer should have been recreated")
	}
	assert.Len(t, li.handlers, 1)
}

func TestLazyInformer_ShutdownSafetyAndRestart(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	gvr := schema.GroupVersionResource{Group: "safe", Version: "v1", Resource: "tests"}

	ctx := t.Context()
	logger := noopLogger()
	li := NewLazyInformer(client, gvr, time.Second, nil, logger, nil)

	// shutdown before start should not panic
	assert.NotPanics(t, li.Shutdown, "shutdown before any AddHandler must be safe")

	// add handler to start informer
	require.NoError(t, li.AddHandler(ctx, "h1", cache.ResourceEventHandlerFuncs{}))
	assert.NotNil(t, li.Informer())
	assert.Len(t, li.handlers, 1)
	assert.NotNil(t, li.cancel)
	assert.NotNil(t, li.done)

	// shutdown while running
	assert.NotPanics(t, li.Shutdown, "shutdown during run must not panic")
	assert.Nil(t, li.Informer())
	assert.Empty(t, li.handlers)
	assert.Nil(t, li.cancel)
	assert.Nil(t, li.done)

	// repeated shutdowns are idempotent
	assert.NotPanics(t, li.Shutdown, "shutdown should be idempotent")

	// re-adding a handler after shutdown must recreate informer and context
	require.NoError(t, li.AddHandler(ctx, "h2", cache.ResourceEventHandlerFuncs{}))
	assert.NotNil(t, li.Informer(), "informer should be recreated after shutdown")
	assert.Len(t, li.handlers, 1)
	assert.NotNil(t, li.cancel)
	assert.NotNil(t, li.done)
}

func TestLazyInformer_EventChannel_Synced(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	ctx := t.Context()
	logger := noopLogger()
	eventCh := make(chan watchtracker.Event, 10)
	li := NewLazyInformer(client, gvr, time.Second, nil, logger, eventCh)

	require.NoError(t, li.AddHandler(ctx, "h1", cache.ResourceEventHandlerFuncs{}))

	select {
	case event := <-eventCh:
		assert.Equal(t, watchtracker.EventTypeSynced, event.Type)
		assert.Equal(t, gvr, event.GVR)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for synced event")
	}
}

func TestLazyInformer_EventChannel_Cancelled(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	ctx, cancel := context.WithCancel(t.Context())
	logger := noopLogger()
	eventCh := make(chan watchtracker.Event, 10)
	li := NewLazyInformer(client, gvr, time.Second, nil, logger, eventCh)

	// Cancel immediately so sync can't complete
	cancel()

	require.NoError(t, li.AddHandler(ctx, "h1", cache.ResourceEventHandlerFuncs{}))

	// Should not receive synced event since context was cancelled
	select {
	case event := <-eventCh:
		t.Fatalf("unexpected event: %+v", event)
	case <-time.After(100 * time.Millisecond):
		// Expected - no event when cancelled
	}
}

func TestLazyInformer_EventChannel_DelayedSync(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	// Add reactor to delay list by 500ms
	client.PrependReactor("list", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		time.Sleep(500 * time.Millisecond)
		return false, nil, nil // continue to default handler
	})

	ctx := t.Context()
	logger := noopLogger()
	eventCh := make(chan watchtracker.Event, 10)
	li := NewLazyInformer(client, gvr, time.Second, nil, logger, eventCh)

	start := time.Now()
	require.NoError(t, li.AddHandler(ctx, "h1", cache.ResourceEventHandlerFuncs{}))

	// Should NOT receive event before 500ms
	select {
	case event := <-eventCh:
		elapsed := time.Since(start)
		if elapsed < 400*time.Millisecond {
			t.Fatalf("received event too early (%v): %+v", elapsed, event)
		}
		assert.Equal(t, watchtracker.EventTypeSynced, event.Type)
	case <-time.After(1000 * time.Millisecond):
		t.Fatal("timed out waiting for synced event")
	}
}
