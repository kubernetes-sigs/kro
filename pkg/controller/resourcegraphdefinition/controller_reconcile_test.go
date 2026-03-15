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

package resourcegraphdefinition

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

func TestBuildResourceInfo(t *testing.T) {
	assert.Equal(t, v1alpha1.ResourceInformation{
		ID: "subnet",
		Dependencies: []v1alpha1.Dependency{
			{ID: "vpc"},
			{ID: "internetGateway"},
		},
	}, buildResourceInfo("subnet", []string{"vpc", "internetGateway"}))
}

func TestErrorWrappers(t *testing.T) {
	boom := errors.New("boom")

	tests := []struct {
		name     string
		build    func(error) error
		assertAs func(*testing.T, error)
	}{
		{
			name:  "graph errors unwrap correctly",
			build: newGraphError,
			assertAs: func(t *testing.T, err error) {
				var graphErr *graphError
				require.ErrorAs(t, err, &graphErr)
			},
		},
		{
			name:  "crd errors unwrap correctly",
			build: newCRDError,
			assertAs: func(t *testing.T, err error) {
				var crdErr *crdError
				require.ErrorAs(t, err, &crdErr)
			},
		},
		{
			name:  "microcontroller errors unwrap correctly",
			build: newMicroControllerError,
			assertAs: func(t *testing.T, err error) {
				var microErr *microControllerError
				require.ErrorAs(t, err, &microErr)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.build(boom)
			assert.ErrorIs(t, err, boom)
			assert.Equal(t, "boom", err.Error())
			tt.assertAs(t, err)
		})
	}
}

func TestReconcileResourceGraphDefinitionGraphStableOrder(t *testing.T) {
	reconciler := &ResourceGraphDefinitionReconciler{rgBuilder: newTestBuilder()}

	for i := 0; i < 25; i++ {
		processed, resourcesInfo, err := reconciler.reconcileResourceGraphDefinitionGraph(context.Background(), newTestRGD("graph-stable"))
		require.NoError(t, err)
		assert.Equal(t, []string{"vpc", "subnetA", "subnetB"}, processed.TopologicalOrder)
		assert.Equal(t, expectedResourcesInfo(), resourcesInfo)
	}
}

func TestReconcileResourceGraphDefinitionGraphWrapsBuilderErrors(t *testing.T) {
	reconciler := &ResourceGraphDefinitionReconciler{rgBuilder: newFailingBuilder(errors.New("naming convention violation"))}

	_, _, err := reconciler.reconcileResourceGraphDefinitionGraph(context.Background(), newTestRGD("graph-error"))
	require.Error(t, err)

	var graphErr *graphError
	require.ErrorAs(t, err, &graphErr)
}

func TestReconcileResourceGraphDefinition(t *testing.T) {
	tests := []struct {
		name  string
		build func(*testing.T) (*ResourceGraphDefinitionReconciler, *v1alpha1.ResourceGraphDefinition, *stubCRDManager)
		check func(*testing.T, []string, []v1alpha1.ResourceInformation, error, *v1alpha1.ResourceGraphDefinition, *stubCRDManager)
	}{
		{
			name: "successfully reconciles graph crd and microcontroller",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, *v1alpha1.ResourceGraphDefinition, *stubCRDManager) {
				rgd := newTestRGD("rgd-success")
				rgd.Annotations = map[string]string{
					v1alpha1.AllowBreakingChangesAnnotation: "true",
				}

				manager := &stubCRDManager{}
				return &ResourceGraphDefinitionReconciler{
					rgBuilder:         newTestBuilder(),
					dynamicController: newRunningDynamicController(t),
					crdManager:        manager,
					clientSet:         newKROFakeSet(),
					instanceLogger:    logr.Discard(),
				}, rgd, manager
			},
			check: func(t *testing.T, topologicalOrder []string, resourcesInfo []v1alpha1.ResourceInformation, err error, rgd *v1alpha1.ResourceGraphDefinition, manager *stubCRDManager) {
				require.NoError(t, err)
				assert.Equal(t, []string{"vpc", "subnetA", "subnetB"}, topologicalOrder)
				assert.Equal(t, expectedResourcesInfo(), resourcesInfo)
				assert.True(t, manager.lastAllowBreaking)
				assert.Equal(t, "true", manager.lastEnsure.Labels[metadata.OwnedLabel])
				assert.Equal(t, rgd.Name, manager.lastEnsure.Labels[metadata.ResourceGraphDefinitionNameLabel])
				assert.Equal(t, string(rgd.UID), manager.lastEnsure.Labels[metadata.ResourceGraphDefinitionIDLabel])
				assert.True(t, conditionFor(t, rgd, ResourceGraphAccepted).IsTrue())
				assert.True(t, conditionFor(t, rgd, KindReady).IsTrue())
				assert.True(t, conditionFor(t, rgd, ControllerReady).IsTrue())
				assert.True(t, rgdConditionTypes.For(rgd).IsRootReady())
			},
		},
		{
			name: "returns graph errors and marks the graph invalid",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, *v1alpha1.ResourceGraphDefinition, *stubCRDManager) {
				rgd := newTestRGD("rgd-graph-error")
				return &ResourceGraphDefinitionReconciler{
					rgBuilder: newFailingBuilder(errors.New("naming convention violation")),
				}, rgd, nil
			},
			check: func(t *testing.T, topologicalOrder []string, resourcesInfo []v1alpha1.ResourceInformation, err error, rgd *v1alpha1.ResourceGraphDefinition, _ *stubCRDManager) {
				require.Error(t, err)
				assert.Nil(t, topologicalOrder)
				assert.Nil(t, resourcesInfo)

				var graphErr *graphError
				require.ErrorAs(t, err, &graphErr)
				assert.True(t, conditionFor(t, rgd, ResourceGraphAccepted).IsFalse())
			},
		},
		{
			name: "returns crd errors and preserves graph output",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, *v1alpha1.ResourceGraphDefinition, *stubCRDManager) {
				rgd := newTestRGD("rgd-crd-error")
				manager := &stubCRDManager{ensureErr: errors.New("crd boom")}
				return &ResourceGraphDefinitionReconciler{
					rgBuilder:         newTestBuilder(),
					dynamicController: newRunningDynamicController(t),
					crdManager:        manager,
					clientSet:         newKROFakeSet(),
					instanceLogger:    logr.Discard(),
				}, rgd, manager
			},
			check: func(t *testing.T, topologicalOrder []string, resourcesInfo []v1alpha1.ResourceInformation, err error, rgd *v1alpha1.ResourceGraphDefinition, _ *stubCRDManager) {
				require.Error(t, err)
				assert.Equal(t, []string{"vpc", "subnetA", "subnetB"}, topologicalOrder)
				assert.Equal(t, expectedResourcesInfo(), resourcesInfo)

				var crdErr *crdError
				require.ErrorAs(t, err, &crdErr)
				assert.True(t, conditionFor(t, rgd, KindReady).IsFalse())
			},
		},
		{
			name: "continues when the crd fetch fails after ensure",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, *v1alpha1.ResourceGraphDefinition, *stubCRDManager) {
				rgd := newTestRGD("rgd-crd-get-error")
				manager := &stubCRDManager{getErr: errors.New("crd get boom")}
				return &ResourceGraphDefinitionReconciler{
					rgBuilder:         newTestBuilder(),
					dynamicController: newRunningDynamicController(t),
					crdManager:        manager,
					clientSet:         newKROFakeSet(),
					instanceLogger:    logr.Discard(),
				}, rgd, manager
			},
			check: func(t *testing.T, topologicalOrder []string, resourcesInfo []v1alpha1.ResourceInformation, err error, rgd *v1alpha1.ResourceGraphDefinition, _ *stubCRDManager) {
				require.NoError(t, err)
				assert.Equal(t, []string{"vpc", "subnetA", "subnetB"}, topologicalOrder)
				assert.Equal(t, expectedResourcesInfo(), resourcesInfo)
				assert.True(t, conditionFor(t, rgd, KindReady).IsFalse())
				assert.True(t, conditionFor(t, rgd, ControllerReady).IsTrue())
				assert.False(t, rgdConditionTypes.For(rgd).IsRootReady())
			},
		},
		{
			name: "returns microcontroller registration errors",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, *v1alpha1.ResourceGraphDefinition, *stubCRDManager) {
				rgd := newTestRGD("rgd-micro-error")
				return &ResourceGraphDefinitionReconciler{
					rgBuilder:         newTestBuilder(),
					dynamicController: newDynamicController(t),
					crdManager:        &stubCRDManager{},
					clientSet:         newKROFakeSet(),
					instanceLogger:    logr.Discard(),
				}, rgd, nil
			},
			check: func(t *testing.T, topologicalOrder []string, resourcesInfo []v1alpha1.ResourceInformation, err error, _ *v1alpha1.ResourceGraphDefinition, _ *stubCRDManager) {
				require.Error(t, err)
				assert.Equal(t, []string{"vpc", "subnetA", "subnetB"}, topologicalOrder)
				assert.Equal(t, expectedResourcesInfo(), resourcesInfo)

				var microErr *microControllerError
				require.ErrorAs(t, err, &microErr)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reconciler, rgd, manager := tt.build(t)
			topologicalOrder, resourcesInfo, err := reconciler.reconcileResourceGraphDefinition(context.Background(), rgd)
			tt.check(t, topologicalOrder, resourcesInfo, err, rgd, manager)
		})
	}
}
