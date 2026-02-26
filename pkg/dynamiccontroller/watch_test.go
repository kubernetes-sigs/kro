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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/metadata/fake"
)

func newTestWatchManager(t *testing.T) *WatchManager {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1.AddMetaToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleMetadataClient(scheme)
	return NewWatchManager(client, 1*time.Hour, func(Event) {}, noopLogger())
}

func TestStopWatch_Idempotent(t *testing.T) {
	wm := newTestWatchManager(t)
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	wm.EnsureWatch(gvr)
	assert.Equal(t, 1, wm.ActiveWatchCount())

	wm.StopWatch(gvr)
	assert.Equal(t, 0, wm.ActiveWatchCount())

	// Second StopWatch should not panic and count stays 0.
	wm.StopWatch(gvr)
	assert.Equal(t, 0, wm.ActiveWatchCount())
}

func TestStopWatch_ThenEnsureWatch_CreatesFresh(t *testing.T) {
	wm := newTestWatchManager(t)
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	wm.EnsureWatch(gvr)
	inf1 := wm.GetInformer(gvr)
	assert.NotNil(t, inf1)

	wm.StopWatch(gvr)
	assert.Nil(t, wm.GetInformer(gvr))

	wm.EnsureWatch(gvr)
	inf2 := wm.GetInformer(gvr)
	assert.NotNil(t, inf2)

	// Must be a new informer instance, not the old one.
	assert.NotSame(t, inf1, inf2, "expected fresh informer after StopWatch + EnsureWatch")
}
