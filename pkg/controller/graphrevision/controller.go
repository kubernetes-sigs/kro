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

package graphrevision

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlrtcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	internalv1alpha1 "github.com/kubernetes-sigs/kro/api/internal.kro.run/v1alpha1"
	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	graphhash "github.com/kubernetes-sigs/kro/pkg/graph/hash"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
)

type compileGraphFunc func(*krov1alpha1.ResourceGraphDefinition, graph.RGDConfig) (*graph.Graph, error)

// GraphRevisionReconciler reconciles GraphRevision objects.
type GraphRevisionReconciler struct {
	client.Client
	compileGraph            compileGraphFunc
	registry                *revisions.Registry
	rgdConfig               graph.RGDConfig
	maxConcurrentReconciles int
}

// NewGraphRevisionReconciler creates a reconciler that compiles GraphRevision
// snapshots and stores the results in the shared in-memory registry.
func NewGraphRevisionReconciler(
	rgBuilder *graph.Builder,
	registry *revisions.Registry,
	maxConcurrentReconciles int,
	rgdConfig graph.RGDConfig,
) *GraphRevisionReconciler {
	return &GraphRevisionReconciler{
		compileGraph:            rgBuilder.NewResourceGraphDefinition,
		registry:                registry,
		rgdConfig:               rgdConfig,
		maxConcurrentReconciles: maxConcurrentReconciles,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *GraphRevisionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()

	return ctrl.NewControllerManagedBy(mgr).
		For(&internalv1alpha1.GraphRevision{}).
		WithOptions(ctrlrtcontroller.Options{
			MaxConcurrentReconciles: r.maxConcurrentReconciles,
		}).
		Complete(reconcile.AsReconciler(mgr.GetClient(), r))
}

// Reconcile compiles GraphRevision definitions, tracks readiness, and stores
// compiled graphs in the in-memory revision registry.
func (r *GraphRevisionReconciler) Reconcile(ctx context.Context, obj *internalv1alpha1.GraphRevision) (ctrl.Result, error) {
	if !obj.DeletionTimestamp.IsZero() {
		if err := r.setUnmanaged(ctx, obj); err != nil {
			return ctrl.Result{}, err
		}
		// Evict only after finalizer removal succeeds. If patching fails, the GR is
		// still present in the API and must remain visible in cache for warmup paths.
		r.registry.Delete(obj.Spec.Snapshot.Name, obj.Spec.Revision)
		return ctrl.Result{}, nil
	}

	if err := r.setManaged(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}

	topologicalOrder, resources, reconcileErr := r.reconcileGraphRevision(ctx, obj)

	if err := r.updateStatus(ctx, obj, topologicalOrder, resources); err != nil {
		if reconcileErr == nil {
			// Revert to Pending — the graph compiled successfully, only the
			// status write failed. Failed would be a lie about the graph's
			// validity and would cause instance controllers to return terminal
			// errors for a graph that actually compiled fine. Pending triggers
			// a requeue, giving the next reconcile a chance to retry the status
			// write.
			specHash, _ := graphhash.Spec(obj.Spec.Snapshot.Spec)
			r.registry.Put(revisions.Entry{
				RGDName:  obj.Spec.Snapshot.Name,
				Revision: obj.Spec.Revision,
				SpecHash: specHash,
				State:    revisions.RevisionStatePending,
			})
		}
		reconcileErr = errors.Join(reconcileErr, err)
	}

	return ctrl.Result{}, reconcileErr
}

func (r *GraphRevisionReconciler) reconcileGraphRevision(
	ctx context.Context,
	revision *internalv1alpha1.GraphRevision,
) ([]string, []krov1alpha1.ResourceInformation, error) {
	mark := NewConditionsMarkerFor(revision)

	// Compute the spec hash from the snapshot. The hash is an internal
	// implementation detail — it is not persisted on the GraphRevision object.
	specHash, _ := graphhash.Spec(revision.Spec.Snapshot.Spec)

	// Only initialize Pending the first time this revision enters the registry.
	// Re-reconcile should preserve the existing runtime state until compile finishes.
	if _, exists := r.registry.Get(revision.Spec.Snapshot.Name, revision.Spec.Revision); !exists {
		r.registry.Put(revisions.Entry{
			RGDName:  revision.Spec.Snapshot.Name,
			Revision: revision.Spec.Revision,
			SpecHash: specHash,
			State:    revisions.RevisionStatePending,
		})
	}

	compiledGraph, resourcesInfo, err := r.compileGraphRevision(ctx, revision)
	if err != nil {
		mark.GraphInvalid(err.Error())
		// Keep failed state in-memory for this revision number so callers stop
		// waiting on it and can surface a terminal result.
		r.registry.Put(revisions.Entry{
			RGDName:  revision.Spec.Snapshot.Name,
			Revision: revision.Spec.Revision,
			SpecHash: specHash,
			State:    revisions.RevisionStateFailed,
		})
		return nil, nil, err
	}

	mark.GraphVerified()
	// Active is the invariant used by other controllers to mean compiled graph
	// is present and safe to use directly.
	r.registry.Put(revisions.Entry{
		RGDName:       revision.Spec.Snapshot.Name,
		Revision:      revision.Spec.Revision,
		SpecHash:      specHash,
		State:         revisions.RevisionStateActive,
		CompiledGraph: compiledGraph,
	})

	return compiledGraph.TopologicalOrder, resourcesInfo, nil
}

func (r *GraphRevisionReconciler) compileGraphRevision(
	_ context.Context,
	revision *internalv1alpha1.GraphRevision,
) (*graph.Graph, []krov1alpha1.ResourceInformation, error) {
	snapshotRGD := &krov1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: revision.Spec.Snapshot.Name,
		},
		Spec: revision.Spec.Snapshot.Spec,
	}

	compiledGraph, err := r.compileGraph(snapshotRGD, r.rgdConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to compile graph revision %q: %w", revision.Name, err)
	}

	resourcesInfo := make([]krov1alpha1.ResourceInformation, 0, len(compiledGraph.Nodes))
	for _, name := range compiledGraph.TopologicalOrder {
		node := compiledGraph.Nodes[name]
		if deps := node.Meta.Dependencies; len(deps) > 0 {
			resourcesInfo = append(resourcesInfo, buildResourceInfo(name, deps))
		}
	}

	return compiledGraph, resourcesInfo, nil
}

func buildResourceInfo(name string, deps []string) krov1alpha1.ResourceInformation {
	dependencies := make([]krov1alpha1.Dependency, 0, len(deps))
	for _, dep := range deps {
		dependencies = append(dependencies, krov1alpha1.Dependency{ID: dep})
	}
	return krov1alpha1.ResourceInformation{
		ID:           name,
		Dependencies: dependencies,
	}
}
