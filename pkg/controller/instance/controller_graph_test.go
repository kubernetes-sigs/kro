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

	"github.com/stretchr/testify/assert"

	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
)

func TestResolveCompiledGraphUsesNewestIssuedRevisionWhenActive(t *testing.T) {
	latest := &graph.Graph{TopologicalOrder: []string{"latest"}}

	controller := &Controller{
		graphResolver: testRevisionResolver{
			getLatestRevision: func() (revisions.Entry, bool) {
				return revisions.Entry{
					RGDName:       "demo-rgd",
					Revision:      7,
					State:         revisions.RevisionStateActive,
					CompiledGraph: latest,
				}, true
			},
			getGraphRevision: func(int64) (revisions.Entry, bool) { return revisions.Entry{}, false },
		},
	}

	got, err := controller.resolveCompiledGraph()
	assert.NoError(t, err)
	assert.Same(t, latest, got)
}

func TestResolveCompiledGraphRequeuesWhenLatestIsPending(t *testing.T) {
	controller := &Controller{
		graphResolver: testRevisionResolver{
			getLatestRevision: func() (revisions.Entry, bool) {
				return revisions.Entry{
					RGDName:  "demo-rgd",
					Revision: 8,
					State:    revisions.RevisionStatePending,
				}, true
			},
			getGraphRevision: func(int64) (revisions.Entry, bool) { return revisions.Entry{}, false },
		},
		reconcileConfig: ReconcileConfig{DefaultRequeueDuration: 3},
	}

	_, err := controller.resolveCompiledGraph()
	assert.Error(t, err)
	_, ok := err.(*requeue.RequeueNeededAfter)
	assert.True(t, ok)
}

func TestResolveCompiledGraphNoRequeueWhenLatestFailed(t *testing.T) {
	controller := &Controller{
		graphResolver: testRevisionResolver{
			getLatestRevision: func() (revisions.Entry, bool) {
				return revisions.Entry{
					RGDName:  "demo-rgd",
					Revision: 8,
					State:    revisions.RevisionStateFailed,
				}, true
			},
			getGraphRevision: func(int64) (revisions.Entry, bool) { return revisions.Entry{}, false },
		},
	}

	_, err := controller.resolveCompiledGraph()
	assert.Error(t, err)
	_, ok := err.(*requeue.NoRequeue)
	assert.True(t, ok)
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
