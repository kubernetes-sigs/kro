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
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func noopLogger() logr.Logger {
	opts := zap.Options{DestWriter: io.Discard}
	return zap.New(zap.UseFlagOptions(&opts))
}

type addErrorInformer struct {
	cache.SharedIndexInformer
	addErr error
}

func (a *addErrorInformer) AddEventHandler(cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	return nil, a.addErr
}

type removeErrorInformer struct {
	cache.SharedIndexInformer
	removeErr error
}

func (r *removeErrorInformer) RemoveEventHandler(cache.ResourceEventHandlerRegistration) error {
	return r.removeErr
}

func TestLazyInformer_AddAndRemoveHandler(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	logger := noopLogger()
	li := NewLazyInformer(client, gvr, time.Second, nil, logger)

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
	li := NewLazyInformer(client, gvr, time.Second, nil, logger)

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
	li := NewLazyInformer(client, gvr, time.Second, nil, logger)

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
	li := NewLazyInformer(client, gvr, time.Second, nil, logger) // avoid tiny resync

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
	li := NewLazyInformer(client, gvr, time.Second, nil, logger)

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

func TestLazyInformer_EnsureInformerAlreadyExists(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	ctx := t.Context()
	logger := noopLogger()
	li := NewLazyInformer(client, gvr, time.Second, nil, logger)

	require.NoError(t, li.AddHandler(ctx, "h1", cache.ResourceEventHandlerFuncs{}))
	firstInformer := li.Informer()
	assert.NotNil(t, firstInformer)

	require.NoError(t, li.AddHandler(ctx, "h2", cache.ResourceEventHandlerFuncs{}))
	secondInformer := li.Informer()
	assert.Same(t, firstInformer, secondInformer)
	assert.Len(t, li.handlers, 2)
}

func TestLazyInformer_RemoveNonExistentHandler(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	ctx := t.Context()
	logger := noopLogger()
	li := NewLazyInformer(client, gvr, time.Second, nil, logger)

	require.NoError(t, li.AddHandler(ctx, "h1", cache.ResourceEventHandlerFuncs{}))
	assert.Len(t, li.handlers, 1)

	stopped, err := li.RemoveHandler("nonexistent")
	assert.NoError(t, err)
	assert.False(t, stopped)
	assert.Len(t, li.handlers, 1)
	assert.NotNil(t, li.Informer())
}

func TestLazyInformer_AddHandler_ErrorFromInformer(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	ctx := t.Context()
	logger := noopLogger()
	li := NewLazyInformer(client, gvr, time.Second, nil, logger)
	t.Cleanup(li.Shutdown)

	li.ensureInformer()
	li.informer = &addErrorInformer{
		SharedIndexInformer: li.informer,
		addErr:              fmt.Errorf("add failed"),
	}

	err := li.AddHandler(ctx, "h1", cache.ResourceEventHandlerFuncs{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "add failed")
	assert.Empty(t, li.handlers)
}

func TestLazyInformer_RemoveHandler_ErrorFromInformer(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	ctx := t.Context()
	logger := noopLogger()
	li := NewLazyInformer(client, gvr, time.Second, nil, logger)
	t.Cleanup(li.Shutdown)

	require.NoError(t, li.AddHandler(ctx, "h1", cache.ResourceEventHandlerFuncs{}))

	li.informer = &removeErrorInformer{
		SharedIndexInformer: li.informer,
		removeErr:           fmt.Errorf("remove failed"),
	}

	stopped, err := li.RemoveHandler("h1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "remove failed")
	assert.False(t, stopped)
	assert.Len(t, li.handlers, 1)
}

func TestLazyInformer_CacheSyncFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	logger := noopLogger()
	li := NewLazyInformer(client, gvr, time.Second, nil, logger)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := li.AddHandler(ctx, "h1", cache.ResourceEventHandlerFuncs{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to sync")
	assert.Nil(t, li.Informer())
	assert.Empty(t, li.handlers)
}

func TestLazyInformer_InformerWithTweak(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	logger := noopLogger()
	tweak := func(opts *v1.ListOptions) {
		opts.LabelSelector = "test=value"
	}

	li := NewLazyInformer(client, gvr, time.Second, tweak, logger)

	assert.NotNil(t, li.tweak)

	ctx := t.Context()
	_ = li.AddHandler(ctx, "h1", cache.ResourceEventHandlerFuncs{})
}

func TestLazyInformer_WatchError(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)

	fakeWatch := watch.NewFake()
	client.PrependWatchReactor("tests", func(action k8stesting.Action) (handled bool, ret watch.Interface, err error) {
		go func() {
			time.Sleep(10 * time.Millisecond)
			fakeWatch.Error(&v1.Status{
				Status:  "Failure",
				Message: "simulated watch error",
			})
		}()
		return true, fakeWatch, nil
	})

	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	logger := noopLogger()
	li := NewLazyInformer(client, gvr, time.Second, nil, logger)

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	err := li.AddHandler(ctx, "h1", cache.ResourceEventHandlerFuncs{})
	if err != nil {
		assert.Contains(t, err.Error(), "failed to sync")
	}
}

func TestLazyInformer_RemoveHandler_InformerNil(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	gvr := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

	logger := noopLogger()
	li := NewLazyInformer(client, gvr, time.Second, nil, logger)

	ctx := t.Context()
	require.NoError(t, li.AddHandler(ctx, "h1", cache.ResourceEventHandlerFuncs{}))

	li.mu.Lock()
	li.informer = nil
	li.mu.Unlock()

	stopped, err := li.RemoveHandler("h1")
	assert.NoError(t, err)
	assert.False(t, stopped)
}
