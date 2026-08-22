// Copyright 2026 The Kubernetes Authors.
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
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// statusesMatch guards against an infinite reconcile loop: an unconditional
// UpdateStatus bumps resourceVersion, which fires a watch event, which
// re-enqueues the instance. The comparison must therefore survive an API
// round-trip, where apimachinery decodes persisted whole numbers as int64
// while CEL evaluation produces float64.
func TestStatusesMatchSurvivesAPIRoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		wire  map[string]interface{}
		want  map[string]interface{}
		match bool
	}{
		{
			name:  "whole float from CEL equals int64 from the wire",
			wire:  map[string]interface{}{"replicas": int64(3)},
			want:  map[string]interface{}{"replicas": float64(3)},
			match: true,
		},
		{
			name:  "nested whole float equals nested int64",
			wire:  map[string]interface{}{"scale": map[string]interface{}{"desired": int64(2)}},
			want:  map[string]interface{}{"scale": map[string]interface{}{"desired": float64(2)}},
			match: true,
		},
		{
			name:  "whole float inside a slice equals int64",
			wire:  map[string]interface{}{"ports": []interface{}{int64(80), int64(443)}},
			want:  map[string]interface{}{"ports": []interface{}{float64(80), float64(443)}},
			match: true,
		},
		{
			name:  "fractional value is not equal to its truncation",
			wire:  map[string]interface{}{"ratio": int64(1)},
			want:  map[string]interface{}{"ratio": float64(1.5)},
			match: false,
		},
		{
			name:  "different numbers do not match",
			wire:  map[string]interface{}{"replicas": int64(3)},
			want:  map[string]interface{}{"replicas": float64(4)},
			match: false,
		},
		{
			name:  "state is compared as a plain string",
			wire:  map[string]interface{}{"state": "ACTIVE"},
			want:  map[string]interface{}{"state": "ACTIVE"},
			match: true,
		},
		{
			name:  "changed state does not match",
			wire:  map[string]interface{}{"state": "IN_PROGRESS"},
			want:  map[string]interface{}{"state": "ACTIVE"},
			match: false,
		},
		{
			name:  "an added field does not match",
			wire:  map[string]interface{}{"state": "ACTIVE"},
			want:  map[string]interface{}{"state": "ACTIVE", "arn": "arn:aws:s3:::bucket"},
			match: false,
		},
		{
			name:  "a removed field does not match",
			wire:  map[string]interface{}{"state": "ACTIVE", "arn": "arn:aws:s3:::bucket"},
			want:  map[string]interface{}{"state": "ACTIVE"},
			match: false,
		},
		{
			name:  "two empty statuses match",
			wire:  map[string]interface{}{},
			want:  map[string]interface{}{},
			match: true,
		},
		{
			name:  "a nil wire status does not match an empty computed status",
			wire:  nil,
			want:  map[string]interface{}{},
			match: false,
		},
		{
			// json.Marshal sorts map keys, so key order on either side is
			// not observable and must never trigger a write.
			name:  "key order is not significant",
			wire:  map[string]interface{}{"a": "1", "b": "2"},
			want:  map[string]interface{}{"b": "2", "a": "1"},
			match: true,
		},
		{
			// A value that cannot be marshalled reports "no match" so the
			// caller falls through to a write, which the API server no-ops.
			// Reporting a match here would silently drop a status update.
			// The symmetric assertion below covers an unmarshalable value on
			// either side of the comparison.
			name:  "an unmarshalable value falls through to a write",
			wire:  map[string]interface{}{"state": "ACTIVE"},
			want:  map[string]interface{}{"state": func() {}},
			match: false,
		},
		{
			// Slices are not sorted, so anything that builds a status list
			// (conditions in particular) has to emit a stable order or the
			// skip-write guard never fires and the loop returns.
			name: "slice order is significant",
			wire: map[string]interface{}{"conditions": []interface{}{
				map[string]interface{}{"type": "Ready"},
				map[string]interface{}{"type": "Synced"},
			}},
			want: map[string]interface{}{"conditions": []interface{}{
				map[string]interface{}{"type": "Synced"},
				map[string]interface{}{"type": "Ready"},
			}},
			match: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.match, statusesMatch(tt.wire, tt.want))
			assert.Equal(t, tt.match, statusesMatch(tt.want, tt.wire),
				"comparison must be symmetric")
		})
	}
}

// countStatusUpdates returns the number of writes to the status subresource
// recorded on the fake client.
func countStatusUpdates(actions []k8stesting.Action) int {
	n := 0
	for _, action := range actions {
		if action.GetVerb() == "update" && action.GetSubresource() == "status" {
			n++
		}
	}
	return n
}

// persistStatus is the only writer of instance status. It must not issue an
// UpdateStatus when the computed status already matches the wire, because the
// resulting resourceVersion bump re-enqueues the instance forever.
func TestPersistStatusSkipsWriteWhenUnchanged(t *testing.T) {
	tests := []struct {
		name        string
		wire        map[string]interface{}
		computed    map[string]interface{}
		wantWrites  int
		description string
	}{
		{
			name:       "identical status is not written",
			wire:       map[string]interface{}{"state": "ACTIVE", "replicas": int64(3)},
			computed:   map[string]interface{}{"state": "ACTIVE", "replicas": int64(3)},
			wantWrites: 0,
		},
		{
			name:       "int64 wire versus float64 CEL output is not written",
			wire:       map[string]interface{}{"state": "ACTIVE", "replicas": int64(3)},
			computed:   map[string]interface{}{"state": "ACTIVE", "replicas": float64(3)},
			wantWrites: 0,
			description: "the round-trip case: the previous reconcile persisted an " +
				"int64 and this one recomputed the same value as a float64",
		},
		{
			name:       "a changed value is written exactly once",
			wire:       map[string]interface{}{"state": "IN_PROGRESS"},
			computed:   map[string]interface{}{"state": "ACTIVE"},
			wantWrites: 1,
		},
		{
			name:       "a first status on an instance with none is written",
			wire:       map[string]interface{}{},
			computed:   map[string]interface{}{"state": "ACTIVE"},
			wantWrites: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newInstanceObject("demo", "default")
			if len(tt.wire) > 0 {
				instance.Object["status"] = tt.wire
			}

			raw := newControllerTestDynamicClient(t, instance)
			client := raw.Resource(controllerTestParentGVR).Namespace("default")
			raw.ClearActions()

			c := &Controller{}
			require.NoError(t, c.persistStatus(
				context.Background(), client, instance, tt.wire, tt.computed, "",
			))

			assert.Equal(t, tt.wantWrites, countStatusUpdates(raw.Actions()), tt.description)
		})
	}
}

// The computed status is mirrored onto the in-memory instance even when the
// API write is skipped, because the deferred event and metric emitters in
// Reconcile read conditions back off the object rather than off the marker.
func TestPersistStatusMirrorsStatusOntoInstanceWhenWriteSkipped(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	wire := map[string]interface{}{"state": "ACTIVE"}
	instance.Object["status"] = wire

	raw := newControllerTestDynamicClient(t, instance)
	client := raw.Resource(controllerTestParentGVR).Namespace("default")
	raw.ClearActions()

	computed := map[string]interface{}{"state": "ACTIVE"}
	c := &Controller{}
	require.NoError(t, c.persistStatus(
		context.Background(), client, instance, wire, computed, "ACTIVE",
	))

	require.Equal(t, 0, countStatusUpdates(raw.Actions()), "matching status must not be written")
	assert.Equal(t, computed, instance.Object["status"],
		"status must be mirrored onto the instance even when the write is skipped")
}

// A conflict on UpdateStatus is retried against a freshly fetched object, so a
// concurrent write to the instance does not drop kro's status.
func TestPersistStatusRetriesOnConflict(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	raw := newControllerTestDynamicClient(t, instance)

	// Fail the first status write with a conflict, then let it through.
	var attempts int
	raw.PrependReactor("update", "webapps",
		func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
			if action.GetSubresource() != "status" {
				return false, nil, nil
			}
			attempts++
			if attempts == 1 {
				return true, nil, apierrors.NewConflict(
					controllerTestParentGVR.GroupResource(), "demo",
					errors.New("object was modified"),
				)
			}
			return false, nil, nil
		})

	client := raw.Resource(controllerTestParentGVR).Namespace("default")
	c := &Controller{}
	require.NoError(t, c.persistStatus(
		context.Background(), client, instance,
		map[string]interface{}{}, map[string]interface{}{"state": "ACTIVE"}, "",
	))

	assert.Equal(t, 2, attempts, "the conflicting write must be retried")
	stored, err := client.Get(context.Background(), "demo", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "ACTIVE", stored.Object["status"].(map[string]interface{})["state"])
}
