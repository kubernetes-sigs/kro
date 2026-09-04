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

package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The CanWatch gate models the impersonated Graph path: drift-watch
// registration is only attempted when the applying ServiceAccount may watch the
// target GVR. When CanWatch denies (issue #2) the watch is skipped, but the
// object is still applied so the SA-confined apply is unaffected — only drift
// detection is degraded for that GVR.
func TestSimple_CanWatch_DeniedSkipsWatchButApplies(t *testing.T) {
	t.Parallel()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	w := &recordingWatcher{}

	ex := NewSimple(cl)
	ex.CanWatch = func(context.Context, schema.GroupVersionResource, string) (bool, error) {
		return false, nil
	}

	res, err := ex.Apply(context.Background(), compileAndBuild(t, scalarCMGraph("cm")), w)
	require.NoError(t, err, "a denied drift watch must not abort the apply")

	assert.Empty(t, w.reqs, "no watch may be registered when the SA cannot watch the GVR")
	assert.True(t, cmExists(t, cl, "cm"), "the object must still be applied under the confined identity")

	names := make([]string, 0, len(res.Applied))
	for _, a := range res.Applied {
		names = append(names, a.Name)
	}
	assert.Contains(t, names, "cm", "the applied object must still be tracked")
	assert.NotContains(t, res.Unresolved, "cm", "a skipped drift watch must not mark the node unresolved")
}

// An inconclusive CanWatch check (non-nil error) is treated as a skip, not a
// hard failure: the watch is dropped and the apply proceeds.
func TestSimple_CanWatch_InconclusiveSkipsWatchButApplies(t *testing.T) {
	t.Parallel()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	w := &recordingWatcher{}

	ex := NewSimple(cl)
	ex.CanWatch = func(context.Context, schema.GroupVersionResource, string) (bool, error) {
		return false, errors.New("access review unavailable")
	}

	res, err := ex.Apply(context.Background(), compileAndBuild(t, scalarCMGraph("cm")), w)
	require.NoError(t, err, "an inconclusive access review must not abort the apply")

	assert.Empty(t, w.reqs, "an inconclusive access review skips the watch")
	assert.True(t, cmExists(t, cl, "cm"), "the object must still be applied")
	assert.NotContains(t, res.Unresolved, "cm")
}

// When CanWatch permits, the drift watch IS registered as normal.
func TestSimple_CanWatch_AllowedRegistersWatch(t *testing.T) {
	t.Parallel()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	w := &recordingWatcher{}

	var checked []schema.GroupVersionResource
	ex := NewSimple(cl)
	ex.CanWatch = func(_ context.Context, gvr schema.GroupVersionResource, _ string) (bool, error) {
		checked = append(checked, gvr)
		return true, nil
	}

	_, err := ex.Apply(context.Background(), compileAndBuild(t, scalarCMGraph("cm")), w)
	require.NoError(t, err)

	require.Len(t, w.reqs, 1, "a permitted GVR must register its scalar drift watch")
	assert.Equal(t, "cm", w.reqs[0].Name)
	assert.Equal(t, schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, w.reqs[0].GVR)
	require.NotEmpty(t, checked, "CanWatch must be consulted for the target GVR")
	assert.Equal(t, schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, checked[0])
}

// A nil CanWatch (the RGD/instance path, controller identity) leaves the gate
// inert: every watch is attempted exactly as before.
func TestSimple_CanWatch_NilGateAttemptsEveryWatch(t *testing.T) {
	t.Parallel()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	w := &recordingWatcher{}

	_, err := NewSimple(cl).Apply(context.Background(),
		compileAndBuild(t, scalarCMGraph("cm")), w)
	require.NoError(t, err)

	require.Len(t, w.reqs, 1, "with no gate, the watch is registered unconditionally")
	assert.Equal(t, "cm", w.reqs[0].Name)
}
