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

package resourcegraphdefinition

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlrtcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	internalv1alpha1 "github.com/kubernetes-sigs/kro/api/internal.kro.run/v1alpha1"
	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	kroclient "github.com/kubernetes-sigs/kro/pkg/client"
	"github.com/kubernetes-sigs/kro/pkg/dynamiccontroller"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

type resourceGraphBuilder interface {
	NewResourceGraphDefinition(*v1alpha1.ResourceGraphDefinition, graph.RGDConfig) (*graph.Graph, error)
}

// Config holds tunable parameters for the RGD reconciler.
type Config struct {
	AllowCRDDeletion        bool
	InstanceRequeueInterval time.Duration
	ProgressRequeueDelay    time.Duration
	MaxConcurrentReconciles int
	MaxGraphRevisions       int
	RGDConfig               graph.RGDConfig
}

// ResourceGraphDefinitionReconciler reconciles a ResourceGraphDefinition object
type ResourceGraphDefinitionReconciler struct {
	// Client, apiReader, and instanceLogger are set with SetupWithManager
	client.Client
	apiReader      client.Reader
	instanceLogger logr.Logger

	clientSet         kroclient.SetInterface
	crdManager        kroclient.CRDClient
	metadataLabeler   metadata.Labeler
	rgBuilder         resourceGraphBuilder
	dynamicController *dynamiccontroller.DynamicController
	revisionsRegistry *revisions.Registry
	cfg               Config

	newEventRecorder func(string) record.EventRecorder
}

func NewResourceGraphDefinitionReconciler(
	clientSet kroclient.SetInterface,
	dynamicController *dynamiccontroller.DynamicController,
	builder *graph.Builder,
	revisionsRegistry *revisions.Registry,
	cfg Config,
) *ResourceGraphDefinitionReconciler {
	crdWrapper := clientSet.CRD(kroclient.CRDWrapperConfig{})

	return &ResourceGraphDefinitionReconciler{
		clientSet:         clientSet,
		crdManager:        crdWrapper,
		dynamicController: dynamicController,
		revisionsRegistry: revisionsRegistry,
		metadataLabeler:   metadata.NewKROMetaLabeler(),
		rgBuilder:         builder,
		cfg:               cfg,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceGraphDefinitionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	// GraphRevision selection relies on a CRD selectable field, so use the
	// direct API reader instead of a cache-only field index.
	r.apiReader = mgr.GetAPIReader()
	r.clientSet.SetRESTMapper(mgr.GetRESTMapper())
	r.instanceLogger = mgr.GetLogger()
	r.newEventRecorder = mgr.GetEventRecorderFor

	logConstructor := func(req *reconcile.Request) logr.Logger {
		log := mgr.GetLogger().WithName("rgd-controller").WithValues(
			"controller", "ResourceGraphDefinition",
			"controllerGroup", v1alpha1.GroupVersion.Group,
			"controllerKind", "ResourceGraphDefinition",
		)
		if req != nil {
			log = log.WithValues("name", req.Name)
		}
		return log
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("ResourceGraphDefinition").
		For(
			&v1alpha1.ResourceGraphDefinition{},
			builder.WithPredicates(
				predicate.Or(
					resourceGraphDefinitionPrimaryWatchPredicate(),
					annotationChangedPredicate(),
				),
			),
		).
		WithOptions(
			ctrlrtcontroller.Options{
				LogConstructor:          logConstructor,
				MaxConcurrentReconciles: r.cfg.MaxConcurrentReconciles,
			},
		).
		Owns(&internalv1alpha1.GraphRevision{}).
		Watches(
			&internalv1alpha1.GraphRevision{},
			handler.EnqueueRequestsFromMapFunc(r.findRGDsForGraphRevision),
		).
		WatchesMetadata(
			&extv1.CustomResourceDefinition{},
			handler.EnqueueRequestsFromMapFunc(r.findRGDsForCRD),
			builder.WithPredicates(predicate.Funcs{
				UpdateFunc: func(e event.UpdateEvent) bool {
					return true
				},
				CreateFunc: func(e event.CreateEvent) bool {
					return false
				},
				DeleteFunc: func(e event.DeleteEvent) bool {
					return true
				},
			}),
		).
		Complete(reconcile.AsReconciler[*v1alpha1.ResourceGraphDefinition](mgr.GetClient(), r))
}

// resourceGraphDefinitionPrimaryWatchPredicate returns a predicate that decides
// which ResourceGraphDefinition events trigger a reconcile.
//
// The default GenerationChangedPredicate is insufficient here because Kubernetes
// does NOT bump .metadata.generation when .metadata.deletionTimestamp is set.
// That means a plain generation check silently drops the update that kicks off
// finalizer-driven cleanup, and the controller never runs its delete path until
// the final delete event — by which point the object is already gone from the API
// server.
//
// This predicate reconciles when:
//   - spec changes  (generation changed), or
//   - deletion begins (deletionTimestamp transitions from zero to non-zero).
//
// It skips:
//   - status-only updates (generation and deletion state unchanged),
//   - delete events (object is already removed; nothing left to reconcile).
func resourceGraphDefinitionPrimaryWatchPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return false
			}

			oldDeleting := !e.ObjectOld.GetDeletionTimestamp().IsZero()
			newDeleting := !e.ObjectNew.GetDeletionTimestamp().IsZero()
			return e.ObjectNew.GetGeneration() != e.ObjectOld.GetGeneration() || oldDeleting != newDeleting
		},
		DeleteFunc: func(event.DeleteEvent) bool {
			return false
		},
	}
}

// annotationChangedPredicate triggers a reconcile only when the
// allow-breaking-changes annotation is changed between "true" and "false"
// Adding the annotation as "false" from no annotation or vice versa, is the same
// and we do not trigger a reconcile.
// Delete events are suppressed to avoid redundant reconciles
func annotationChangedPredicate() predicate.Predicate {
	return predicate.TypedFuncs[client.Object]{
		UpdateFunc: func(e event.TypedUpdateEvent[client.Object]) bool {
			oldVal := e.ObjectOld.GetAnnotations()[v1alpha1.AllowBreakingChangesAnnotation] == "true"
			newVal := e.ObjectNew.GetAnnotations()[v1alpha1.AllowBreakingChangesAnnotation] == "true"
			return oldVal != newVal
		},
		DeleteFunc: func(event.TypedDeleteEvent[client.Object]) bool {
			return false
		},
	}
}

// findRGDsForCRD returns a list of reconcile requests for the ResourceGraphDefinition
// that owns the given CRD. It is used to trigger reconciliation when a CRD is updated.
func (r *ResourceGraphDefinitionReconciler) findRGDsForCRD(ctx context.Context, obj client.Object) []reconcile.Request {
	mobj, err := meta.Accessor(obj)
	if err != nil {
		return nil
	}

	// Check if the CRD is owned by a ResourceGraphDefinition
	if !metadata.IsKROOwned(mobj) {
		return nil
	}

	rgdName, ok := mobj.GetLabels()[metadata.ResourceGraphDefinitionNameLabel]
	if !ok {
		return nil
	}

	// Return a reconcile request for the corresponding RGD
	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Name: rgdName,
			},
		},
	}
}

// findRGDsForGraphRevision enqueues the RGD named in spec.snapshot.name.
//
// This watch supplements .Owns() which only covers GRs with an ownerReference.
// Orphaned revisions (ownerRef stripped or never set) still carry the RGD name
// in their spec, so this watch ensures the RGD is notified and can adopt them.
// Together the two watches cover both identity axes: ownerRef (GC/lifecycle)
// and spec.snapshot.name (logical grouping used by listGraphRevisions).
func (r *ResourceGraphDefinitionReconciler) findRGDsForGraphRevision(_ context.Context, obj client.Object) []reconcile.Request {
	gr, ok := obj.(*internalv1alpha1.GraphRevision)
	if !ok {
		return nil
	}
	if gr.Spec.Snapshot.Name == "" {
		return nil
	}
	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: gr.Spec.Snapshot.Name}},
	}
}

func (r *ResourceGraphDefinitionReconciler) Reconcile(
	ctx context.Context,
	o *v1alpha1.ResourceGraphDefinition,
) (ctrl.Result, error) {
	if !o.DeletionTimestamp.IsZero() {
		// Block deletion while instances of the generated CRD still exist.
		// Shutting down the instance controller before instances are gone
		// leaves them with a dangling finalizer that no controller can remove.
		gvr := metadata.GetResourceGraphDefinitionInstanceGVR(
			o.Spec.Schema.Group, o.Spec.Schema.APIVersion, o.Spec.Schema.Kind,
		)
		existing, err := r.clientSet.Dynamic().Resource(gvr).List(ctx, metav1.ListOptions{Limit: 1})
		if err != nil {
			// If the resource type is gone (CRD already deleted externally),
			// there can't be any instances — safe to proceed with cleanup.
			if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
				return ctrl.Result{}, fmt.Errorf("failed to check for existing instances: %w", err)
			}
		} else if len(existing.Items) > 0 {
			ctrl.LoggerFrom(ctx).Info(
				"waiting for instances to be deleted before cleaning up ResourceGraphDefinition",
				"instanceGVR", gvr.String(),
			)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		startTime := time.Now()
		if err := r.cleanupResourceGraphDefinition(ctx, o); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.setUnmanaged(ctx, o); err != nil {
			return ctrl.Result{}, err
		}
		deletionDuration.WithLabelValues(o.Name).Observe(time.Since(startTime).Seconds())
		deletionsTotal.WithLabelValues(o.Name).Inc()
		return ctrl.Result{}, nil
	}

	if err := r.setManaged(ctx, o); err != nil {
		return ctrl.Result{}, err
	}

	reconcileResult, topologicalOrder, resourcesInformation, reconcileErr := r.reconcileResourceGraphDefinition(ctx, o)

	if err := r.updateStatus(ctx, o, topologicalOrder, resourcesInformation); err != nil {
		reconcileErr = errors.Join(reconcileErr, err)
	}

	return reconcileResult, reconcileErr
}
