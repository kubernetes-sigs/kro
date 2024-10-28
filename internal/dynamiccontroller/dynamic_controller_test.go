package dynamiccontroller

import (
	"context"
	"io/ioutil"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
		DestWriter: ioutil.Discard,
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

func TestEnqueueObject(t *testing.T) {
	logger := noopLogger()
	client := setupFakeClient()
	dc := NewDynamicController(logger, Config{}, client)

	obj := &unstructured.Unstructured{}
	obj.SetName("test-object")
	obj.SetNamespace("default")
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "test", Version: "v1", Kind: "Test"})

	dc.enqueueObject(obj, "add")

	assert.Equal(t, 1, dc.queue.Len())
}
