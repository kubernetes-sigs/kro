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

package instance

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/kubernetes-sigs/kro/pkg/dynamiccontroller"
)

// mockWatcher captures Watch calls for assertions.
type mockWatcher struct {
	mu       sync.Mutex
	requests []dynamiccontroller.WatchRequest
}

func (m *mockWatcher) Watch(req dynamiccontroller.WatchRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, req)
	return nil
}

func (m *mockWatcher) Done() {}

func (m *mockWatcher) getRequests() []dynamiccontroller.WatchRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]dynamiccontroller.WatchRequest, len(m.requests))
	copy(out, m.requests)
	return out
}

var testGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

func TestRequestWatch_RegistersScalarWatch(t *testing.T) {
	mock := &mockWatcher{}
	rcx := &ReconcileContext{
		Log:     zap.New(zap.UseDevMode(true)),
		Watcher: mock,
	}

	requestWatch(rcx, "deploy", testGVR, "my-deploy", "default")

	reqs := mock.getRequests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "deploy", reqs[0].NodeID)
	assert.Equal(t, testGVR, reqs[0].GVR)
	assert.Equal(t, "my-deploy", reqs[0].Name)
	assert.Equal(t, "default", reqs[0].Namespace)
	assert.Nil(t, reqs[0].Selector)
}

func TestRequestWatch_NilWatcher_NoOp(t *testing.T) {
	rcx := &ReconcileContext{
		Log:     zap.New(zap.UseDevMode(true)),
		Watcher: nil,
	}

	// Should not panic.
	requestWatch(rcx, "deploy", testGVR, "my-deploy", "default")
}

func TestRequestCollectionWatch_RegistersSelectorWatch(t *testing.T) {
	mock := &mockWatcher{}
	rcx := &ReconcileContext{
		Log:     zap.New(zap.UseDevMode(true)),
		Watcher: mock,
	}

	selector, err := labels.Parse("app=my-app")
	require.NoError(t, err)

	cmGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	requestCollectionWatch(rcx, "configs", cmGVR, "default", selector)

	reqs := mock.getRequests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "configs", reqs[0].NodeID)
	assert.Equal(t, cmGVR, reqs[0].GVR)
	assert.Equal(t, "", reqs[0].Name)
	assert.Equal(t, "default", reqs[0].Namespace)
	assert.NotNil(t, reqs[0].Selector)
	assert.Equal(t, "app=my-app", reqs[0].Selector.String())
}

func TestRequestCollectionWatch_NilWatcher_NoOp(t *testing.T) {
	rcx := &ReconcileContext{
		Log:     zap.New(zap.UseDevMode(true)),
		Watcher: nil,
	}

	selector, err := labels.Parse("app=my-app")
	require.NoError(t, err)

	cmGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

	// Should not panic.
	requestCollectionWatch(rcx, "configs", cmGVR, "default", selector)
}
