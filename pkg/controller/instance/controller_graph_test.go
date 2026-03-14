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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
)

func TestResolveCompiledGraphCases(t *testing.T) {
	latest := &graph.Graph{TopologicalOrder: []string{"latest"}}

	tests := []struct {
		name          string
		entry         revisions.Entry
		hasLatest     bool
		requeueAfter  time.Duration
		wantGraph     *graph.Graph
		wantNoRequeue bool
	}{
		{
			name: "uses newest issued revision when active",
			entry: revisions.Entry{
				RGDName:       "demo-rgd",
				Revision:      7,
				State:         revisions.RevisionStateActive,
				CompiledGraph: latest,
			},
			hasLatest: true,
			wantGraph: latest,
		},
		{
			name: "requeues when latest is pending",
			entry: revisions.Entry{
				RGDName:  "demo-rgd",
				Revision: 8,
				State:    revisions.RevisionStatePending,
			},
			hasLatest:    true,
			requeueAfter: 3 * time.Second,
		},
		{
			name: "does not requeue when latest failed",
			entry: revisions.Entry{
				RGDName:  "demo-rgd",
				Revision: 8,
				State:    revisions.RevisionStateFailed,
			},
			hasLatest:     true,
			wantNoRequeue: true,
		},
		{
			name:         "requeues when latest revision is unavailable",
			hasLatest:    false,
			requeueAfter: 2 * time.Second,
		},
		{
			name: "requeues when latest revision state is unknown",
			entry: revisions.Entry{
				RGDName:  "demo-rgd",
				Revision: 9,
				State:    revisions.RevisionState("Unknown"),
			},
			hasLatest:    true,
			requeueAfter: 5 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := &Controller{
				graphResolver: testRevisionResolver{
					getLatestRevision: func() (revisions.Entry, bool) {
						return tt.entry, tt.hasLatest
					},
					getGraphRevision: func(int64) (revisions.Entry, bool) { return revisions.Entry{}, false },
				},
				reconcileConfig: ReconcileConfig{DefaultRequeueDuration: tt.requeueAfter},
			}

			got, err := controller.resolveCompiledGraph()
			if tt.wantGraph != nil {
				require.NoError(t, err)
				assert.Same(t, tt.wantGraph, got)
				return
			}

			require.Error(t, err)
			if tt.wantNoRequeue {
				var noRequeue *requeue.NoRequeue
				require.ErrorAs(t, err, &noRequeue)
				return
			}

			var retryAfter *requeue.RequeueNeededAfter
			require.ErrorAs(t, err, &retryAfter)
			assert.Equal(t, tt.requeueAfter, retryAfter.Duration())
		})
	}
}

type testRevisionResolver struct {
	getLatestRevision func() (revisions.Entry, bool)
	getGraphRevision  func(int64) (revisions.Entry, bool)
}

func (r testRevisionResolver) GetLatestRevision() (revisions.Entry, bool) {
	return r.getLatestRevision()
}

func (r testRevisionResolver) GetGraphRevision(revision int64) (revisions.Entry, bool) {
	return r.getGraphRevision(revision)
}
