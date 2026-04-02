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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlrtcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	internalv1alpha1 "github.com/kubernetes-sigs/kro/api/internal.kro.run/v1alpha1"
	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	graphhash "github.com/kubernetes-sigs/kro/pkg/graph/hash"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/metrics"
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
		For(
			&internalv1alpha1.GraphRevision{},
			builder.WithPredicates(graphRevisionPrimaryWatchPredicate()),
		).
		WithOptions(ctrlrtcontroller.Options{
			MaxConcurrentReconciles: r.maxConcurrentReconciles,
		}).
		Complete(reconcile.AsReconciler(mgr.GetClient(), r))
}

// graphRevisionPrimaryWatchPredicate reconciles only on create and deletion
// start. GraphRevision spec is immutable, so generation-changing updates are
// not part of the normal lifecycle. Status-only and finalizer-only updates are
// ignored so the controller does not re-enqueue itself after persisting its own
// status or finalizer changes.
func graphRevisionPrimaryWatchPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldDeleting := !e.ObjectOld.GetDeletionTimestamp().IsZero()
			newDeleting := !e.ObjectNew.GetDeletionTimestamp().IsZero()
			return !oldDeleting && newDeleting
		},
		GenericFunc: func(event.GenericEvent) bool {
			return false
		},
		DeleteFunc: func(event.DeleteEvent) bool {
			return false
		},
	}
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
		metrics.GraphRevisionFinalizerEvictionsTotal.WithLabelValues().Inc()
		return ctrl.Result{}, nil
	}

	if err := r.setManaged(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}

	topologicalOrder, resources, activeEntry, reconcileErr := r.reconcileGraphRevision(ctx, obj)

	if err := r.updateStatus(ctx, obj, topologicalOrder, resources); err != nil {
		metrics.GraphRevisionStatusUpdateErrorsTotal.WithLabelValues().Inc()
		if reconcileErr == nil && activeEntry != nil {
			metrics.GraphRevisionActivationDeferredTotal.WithLabelValues().Inc()
		}
		reconcileErr = errors.Join(reconcileErr, err)
		return ctrl.Result{}, reconcileErr
	}

	if reconcileErr == nil && activeEntry != nil {
		// Publish Active only after status has been persisted. Until then the
		// revision remains Pending so consumers conservatively requeue.
		r.registry.Put(*activeEntry)
	}

	return ctrl.Result{}, reconcileErr
}

func (r *GraphRevisionReconciler) reconcileGraphRevision(
	ctx context.Context,
	revision *internalv1alpha1.GraphRevision,
) ([]string, []krov1alpha1.ResourceInformation, *revisions.Entry, error) {
	mark := NewConditionsMarkerFor(revision)

	specHash, err := graphhash.Spec(revision.Spec.Snapshot.Spec)
	if err != nil {
		hashErr := fmt.Errorf("compute graph revision spec hash: %w", err)
		mark.GraphInvalid(hashErr.Error())
		return nil, nil, nil, hashErr
	}

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
		return nil, nil, nil, err
	}

	mark.GraphVerified()
	// Return the desired Active entry to the caller, which only publishes it
	// after status has been written successfully.
	return compiledGraph.TopologicalOrder, resourcesInfo, &revisions.Entry{
		RGDName:       revision.Spec.Snapshot.Name,
		Revision:      revision.Spec.Revision,
		SpecHash:      specHash,
		State:         revisions.RevisionStateActive,
		CompiledGraph: compiledGraph,
	}, nil
}

func (r *GraphRevisionReconciler) compileGraphRevision(
	_ context.Context,
	revision *internalv1alpha1.GraphRevision,
) (*graph.Graph, []krov1alpha1.ResourceInformation, error) {
	startTime := time.Now()
	result := "failed"
	defer func() {
		metrics.GraphRevisionCompileDuration.WithLabelValues(result).Observe(time.Since(startTime).Seconds())
		metrics.GraphRevisionCompileTotal.WithLabelValues(result).Inc()
	}()

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

	result = "success"
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
