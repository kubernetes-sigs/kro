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

package graphrevision

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	internalv1alpha1 "github.com/kubernetes-sigs/kro/api/internal.kro.run/v1alpha1"
	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

func TestCompileMetricsTrackSuccessAndFailure(t *testing.T) {
	resetGraphRevisionMetrics()

	successReconciler := &GraphRevisionReconciler{
		compileGraph: func(*v1alpha1.ResourceGraphDefinition, graph.RGDConfig) (*graph.Graph, error) {
			return testCompiledGraph(), nil
		},
		registry:  revisions.NewRegistry(),
		rgdConfig: graph.RGDConfig{},
	}
	_, _, err := successReconciler.compileGraphRevision(context.Background(), newTestGraphRevision("compile-success"))
	require.NoError(t, err)
	assert.Equal(t, 1.0, testutil.ToFloat64(graphRevisionCompileTotal.WithLabelValues("success")))

	failureReconciler := &GraphRevisionReconciler{
		compileGraph: func(*v1alpha1.ResourceGraphDefinition, graph.RGDConfig) (*graph.Graph, error) {
			return nil, errors.New("compile boom")
		},
		registry:  revisions.NewRegistry(),
		rgdConfig: graph.RGDConfig{},
	}
	_, _, err = failureReconciler.compileGraphRevision(context.Background(), newTestGraphRevision("compile-failure"))
	require.Error(t, err)
	assert.Equal(t, 1.0, testutil.ToFloat64(graphRevisionCompileTotal.WithLabelValues("failed")))
}

func TestReconcileMetricsTrackDeferredActivationAndFinalizerEviction(t *testing.T) {
	resetGraphRevisionMetrics()

	revision := newTestGraphRevision("status-patch-fails")
	base := fake.NewClientBuilder().
		WithScheme(graphRevisionTestScheme(t)).
		WithStatusSubresource(&internalv1alpha1.GraphRevision{}).
		WithObjects(revision.DeepCopy()).
		Build()
	reconciler := &GraphRevisionReconciler{
		Client: &statusPatchFailClient{Client: base, patchErr: errors.New("status patch failed")},
		compileGraph: func(*v1alpha1.ResourceGraphDefinition, graph.RGDConfig) (*graph.Graph, error) {
			return testCompiledGraph(), nil
		},
		registry:  revisions.NewRegistry(),
		rgdConfig: graph.RGDConfig{},
	}

	_, err := reconciler.Reconcile(context.Background(), revision.DeepCopy())
	require.Error(t, err)
	assert.Equal(t, 1.0, testutil.ToFloat64(graphRevisionStatusUpdateErrorsTotal.WithLabelValues()))
	assert.Equal(t, 1.0, testutil.ToFloat64(graphRevisionActivationDeferredTotal.WithLabelValues()))

	resetGraphRevisionMetrics()

	deleting := newTestGraphRevision("finalizer-evict")
	metadata.SetGraphRevisionFinalizer(deleting)
	ts := metav1.Now()
	deleting.DeletionTimestamp = &ts
	deleteClient := fake.NewClientBuilder().WithScheme(graphRevisionTestScheme(t)).WithObjects(deleting.DeepCopy()).Build()
	registry := revisions.NewRegistry()
	registry.Put(revisions.Entry{
		RGDName:       deleting.Spec.Snapshot.Name,
		Revision:      deleting.Spec.Revision,
		State:         revisions.RevisionStateActive,
		CompiledGraph: &graph.Graph{},
	})

	reconciler = &GraphRevisionReconciler{
		Client:       deleteClient,
		compileGraph: panicCompile,
		registry:     registry,
		rgdConfig:    graph.RGDConfig{},
	}

	_, err = reconciler.Reconcile(context.Background(), deleting.DeepCopy())
	require.NoError(t, err)
	assert.Equal(t, 1.0, testutil.ToFloat64(graphRevisionFinalizerEvictionsTotal.WithLabelValues()))
}

func resetGraphRevisionMetrics() {
	graphRevisionCompileTotal.Reset()
	graphRevisionCompileDuration.Reset()
	graphRevisionStatusUpdateErrorsTotal.Reset()
	graphRevisionActivationDeferredTotal.Reset()
	graphRevisionFinalizerEvictionsTotal.Reset()
}

func graphRevisionTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, internalv1alpha1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	return scheme
}
