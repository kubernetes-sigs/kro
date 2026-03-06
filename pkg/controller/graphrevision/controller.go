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

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlrtcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
)

type compileGraphFunc func(*v1alpha1.ResourceGraphDefinition, graph.RGDConfig) (*graph.Graph, error)

// GraphRevisionReconciler reconciles GraphRevision objects.
type GraphRevisionReconciler struct {
	client.Client
	compileGraph            compileGraphFunc
	registry                *revisions.Registry
	rgdConfig               graph.RGDConfig
	maxConcurrentReconciles int
}

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

	logConstructor := func(req *reconcile.Request) logr.Logger {
		log := mgr.GetLogger().WithName("graph-revision-controller").WithValues(
			"controller", "GraphRevision",
			"controllerGroup", v1alpha1.GroupVersion.Group,
			"controllerKind", "GraphRevision",
		)
		if req != nil {
			log = log.WithValues("name", req.Name)
		}
		return log
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("GraphRevision").
		For(&v1alpha1.GraphRevision{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		WithOptions(ctrlrtcontroller.Options{
			LogConstructor:          logConstructor,
			MaxConcurrentReconciles: r.maxConcurrentReconciles,
		}).
		Complete(reconcile.AsReconciler[*v1alpha1.GraphRevision](mgr.GetClient(), r))
}

// Reconcile compiles GraphRevision definitions, tracks readiness, and stores
// compiled graphs in the in-memory revision registry.
func (r *GraphRevisionReconciler) Reconcile(ctx context.Context, obj *v1alpha1.GraphRevision) (ctrl.Result, error) {
	if !obj.DeletionTimestamp.IsZero() {
		if err := r.setUnmanaged(ctx, obj); err != nil {
			return ctrl.Result{}, err
		}
		// Evict only after finalizer removal succeeds. If patching fails, the GV is
		// still present in the API and must remain visible in cache for warmup paths.
		r.registry.Delete(obj.Spec.ResourceGraphDefinitionName, obj.Spec.Revision)
		return ctrl.Result{}, nil
	}

	if err := r.setManaged(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}

	topologicalOrder, resources, reconcileErr := r.reconcileGraphRevision(ctx, obj)

	if err := r.updateStatus(ctx, obj, topologicalOrder, resources); err != nil {
		reconcileErr = errors.Join(reconcileErr, err)
	}

	return ctrl.Result{}, reconcileErr
}

func (r *GraphRevisionReconciler) reconcileGraphRevision(
	ctx context.Context,
	revision *v1alpha1.GraphRevision,
) ([]string, []v1alpha1.ResourceInformation, error) {
	mark := NewConditionsMarkerFor(revision)
	// Mark pending before compile so RGD/instance reconcilers can see that this
	// revision exists but is not ready to serve yet.
	r.registry.Put(revisions.Entry{
		RGDName:  revision.Spec.ResourceGraphDefinitionName,
		Revision: revision.Spec.Revision,
		SpecHash: revision.Spec.SpecHash,
		State:    revisions.RevisionStatePending,
	})

	compiledGraph, resourcesInfo, err := r.compileGraphRevision(ctx, revision)
	if err != nil {
		mark.GraphInvalid(err.Error())
		// Keep failed state in-memory for this revision number so callers stop
		// waiting on it and can surface a terminal result.
		r.registry.Put(revisions.Entry{
			RGDName:  revision.Spec.ResourceGraphDefinitionName,
			Revision: revision.Spec.Revision,
			SpecHash: revision.Spec.SpecHash,
			State:    revisions.RevisionStateFailed,
		})
		return nil, nil, err
	}

	mark.GraphVerified()
	// Active is the invariant used by other controllers to mean compiled graph
	// is present and safe to use directly.
	r.registry.Put(revisions.Entry{
		RGDName:       revision.Spec.ResourceGraphDefinitionName,
		Revision:      revision.Spec.Revision,
		SpecHash:      revision.Spec.SpecHash,
		State:         revisions.RevisionStateActive,
		CompiledGraph: compiledGraph,
	})

	return compiledGraph.TopologicalOrder, resourcesInfo, nil
}

func (r *GraphRevisionReconciler) compileGraphRevision(
	_ context.Context,
	revision *v1alpha1.GraphRevision,
) (*graph.Graph, []v1alpha1.ResourceInformation, error) {
	snapshotRGD := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: revision.Spec.ResourceGraphDefinitionName,
			UID:  revision.Spec.ResourceGraphDefinitionUID,
		},
		Spec: revision.Spec.DefinitionSpec,
	}

	compiledGraph, err := r.compileGraph(snapshotRGD, r.rgdConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to compile graph revision %q: %w", revision.Name, err)
	}

	resourcesInfo := make([]v1alpha1.ResourceInformation, 0, len(compiledGraph.Nodes))
	for name, node := range compiledGraph.Nodes {
		if deps := node.Meta.Dependencies; len(deps) > 0 {
			resourcesInfo = append(resourcesInfo, buildResourceInfo(name, deps))
		}
	}

	return compiledGraph, resourcesInfo, nil
}

func buildResourceInfo(name string, deps []string) v1alpha1.ResourceInformation {
	dependencies := make([]v1alpha1.Dependency, 0, len(deps))
	for _, dep := range deps {
		dependencies = append(dependencies, v1alpha1.Dependency{ID: dep})
	}
	return v1alpha1.ResourceInformation{
		ID:           name,
		Dependencies: dependencies,
	}
}
