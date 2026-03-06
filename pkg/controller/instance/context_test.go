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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubernetes-sigs/kro/pkg/requeue"
)

func TestContextHelpersAndStateTransitions(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph())

	require.NotNil(t, controller)
	require.NotNil(t, rcx.Mark)
	require.NotNil(t, rcx.StateManager)

	delayed := rcx.delayedRequeue(errors.New("retry"))
	var retryAfter *requeue.RequeueNeededAfter
	require.ErrorAs(t, delayed, &retryAfter)
	assert.Equal(t, 2*time.Second, retryAfter.Duration())

	clusterScoped := newClusterScopedInstanceObject("cluster-demo")
	_, clusterRCX, raw := newControllerAndContext(t, clusterScoped, newTestGraph())
	_, err := clusterRCX.InstanceClient().Get(context.Background(), clusterScoped.GetName(), metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotNil(t, raw)

	state := &NodeState{}
	state.SetInProgress()
	assert.Equal(t, NodeStateInProgress, state.State)
	state.SetReady()
	assert.Equal(t, NodeStateSynced, state.State)
	state.SetSkipped()
	assert.Equal(t, NodeStateSkipped, state.State)
	state.SetDeleted()
	assert.Equal(t, NodeStateDeleted, state.State)
	state.SetDeleting()
	assert.Equal(t, NodeStateDeleting, state.State)
	state.SetWaitingForReadiness(errors.New("waiting"))
	assert.Equal(t, NodeStateWaitingForReadiness, state.State)
	state.SetError(errors.New("boom"))
	assert.Equal(t, NodeStateError, state.State)

	manager := newStateManager()
	manager.NewNodeState("a").SetReady()
	manager.NewNodeState("b").SetSkipped()
	manager.Update()
	assert.Equal(t, InstanceStateActive, manager.State)

	manager.ReconcileErr = errors.New("boom")
	manager.Update()
	assert.Equal(t, InstanceStateError, manager.State)

	manager.ReconcileErr = requeue.NeededAfter(errors.New("later"), time.Second)
	manager.State = InstanceStateInProgress
	rcx.StateManager = manager
	rcx.updateInstanceState()
	assert.Equal(t, InstanceStateInProgress, rcx.StateManager.State)

	manager = newStateManager()
	manager.NewNodeState("waiting").SetWaitingForReadiness(nil)
	manager.Update()
	assert.Equal(t, InstanceStateInProgress, manager.State)

	manager.State = InstanceStateDeleting
	manager.Update()
	assert.Equal(t, InstanceStateDeleting, manager.State)
}
