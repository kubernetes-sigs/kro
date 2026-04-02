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
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	kroclient "github.com/kubernetes-sigs/kro/pkg/client"
	"github.com/kubernetes-sigs/kro/pkg/dynamiccontroller"
	"github.com/kubernetes-sigs/kro/pkg/features"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/metrics"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
)

// FieldManagerForLabeler is the field manager name used when applying labels.
const FieldManagerForLabeler = "kro.run/labeller"

// ReconcileConfig holds configuration parameters for the reconciliation process.
// It allows the customization of various aspects of the controller's behavior.
type ReconcileConfig struct {
	// DefaultRequeueDuration is the fixed delay used when the instance
	// reconciler needs to retry after transient cluster state changes.
	// Set to 0 to disable delayed requeues.
	DefaultRequeueDuration time.Duration
	// DeletionGraceTimeDuration is the duration to wait after initializing a resource
	// deletion before considering it failed
	// Not implemented.
	DeletionGraceTimeDuration time.Duration
	// DeletionPolicy is the deletion policy to use when deleting resources in the graph
	// TODO(a-hilaly): need to define think the different deletion policies we need to
	// support.
	DeletionPolicy string
	// RGDConfig holds RGD runtime configuration parameters.
	RGDConfig graph.RGDConfig
}

// GraphRevisionResolver resolves compiled graph revisions for a single RGD.
//
// Implementations are already scoped to one RGD's graph revisions. Callers do
// not pass an RGD identifier on each method call; they only ask for the newest
// issued revision or a specific revision number for that RGD.
type GraphRevisionResolver interface {
	// GetLatestRevision returns the newest issued revision currently present in
	// the in-memory registry for this resolver's RGD.
	//
	// The boolean return is false when no revision is currently cached for that
	// RGD, for example before warmup completes or after pruning.
	GetLatestRevision() (revisions.Entry, bool)
	// GetGraphRevision returns a specific revision from the in-memory registry
	// for this resolver's RGD.
	//
	// The boolean return is false when that revision is not present in cache,
	// for example because it has not been warmed yet or has already been pruned.
	GetGraphRevision(revision int64) (revisions.Entry, bool)
}

// Controller manages the reconciliation of a single instance of a ResourceGraphDefinition,
// / it is responsible for reconciling the instance and its sub-resources.
//
// The controller is responsible for the following:
// - Reconciling the instance
// - Reconciling the sub-resources of the instance
// - Updating the status of the instance
// - Managing finalizers, owner references and labels
// - Handling errors and retries
// - Performing cleanup operations (garbage collection)
//
// For each instance of a ResourceGraphDefinition, the controller creates a new instance of
// the InstanceGraphReconciler to manage the reconciliation of the instance and its
// sub-resources.
//
// It is important to state that when the controller is reconciling an instance, it
// creates and uses a new instance of the ResourceGraphDefinitionRuntime to uniquely manage
// the state of the instance and its sub-resources. This ensure that at each
// reconciliation loop, the controller is working with a fresh state of the instance
// and its sub-resources.
// Controller owns reconciliation for instances of a ResourceGraphDefinition.
type Controller struct {
	log    logr.Logger
	client kroclient.SetInterface
	gvr    schema.GroupVersionResource

	graphResolver GraphRevisionResolver
	namespaced    bool

	instanceLabeler      metadata.Labeler
	childResourceLabeler metadata.Labeler
	reconcileConfig      ReconcileConfig
	coordinator          *dynamiccontroller.WatchCoordinator

	// eventRecorder emits K8s Events on condition transitions.
	eventRecorder record.EventRecorder
}

// NewController constructs a new controller that resolves the newest issued
// graph revision for the RGD from a GraphRevisionResolver.
func NewController(
	log logr.Logger,
	reconcileConfig ReconcileConfig,
	gvr schema.GroupVersionResource,
	graphResolver GraphRevisionResolver,
	namespaced bool,
	client kroclient.SetInterface,
	instanceLabeler metadata.Labeler,
	childResourceLabeler metadata.Labeler,
	coord *dynamiccontroller.WatchCoordinator,
	eventRecorder record.EventRecorder,
) *Controller {
	return &Controller{
		log:                  log,
		client:               client,
		gvr:                  gvr,
		graphResolver:        graphResolver,
		namespaced:           namespaced,
		instanceLabeler:      instanceLabeler,
		childResourceLabeler: childResourceLabeler,
		reconcileConfig:      reconcileConfig,
		coordinator:          coord,
		eventRecorder:        eventRecorder,
	}
}

// Reconcile implements the controller-runtime Reconcile interface.
func (c *Controller) Reconcile(ctx context.Context, req ctrl.Request) (err error) {
	log := c.log.WithValues("namespace", req.Namespace, "name", req.Name)

	// Get per-instance watcher from the coordinator.
	watcher := c.coordinator.ForInstance(c.gvr, req.NamespacedName)

	start := time.Now()
	defer func() {
		watcher.Done(err == nil || requeue.IsRequeueError(err))
		gvr := c.gvr.String()
		metrics.InstanceReconcileDurationSeconds.WithLabelValues(gvr).Observe(time.Since(start).Seconds())
		metrics.InstanceReconcileTotal.WithLabelValues(gvr).Inc()
		if err != nil && !requeue.IsRequeueError(err) {
			log.V(1).Info("reporting reconcile error metric", "error", err)
			metrics.InstanceReconcileErrorsTotal.WithLabelValues(gvr).Inc()
		}
	}()

	//--------------------------------------------------------------
	// 1. Load instance; snapshot conditions for event diff
	//--------------------------------------------------------------
	ri := c.client.Dynamic().Resource(c.gvr)
	var inst *unstructured.Unstructured
	if c.namespaced {
		inst, err = ri.Namespace(req.Namespace).Get(ctx, req.Name, metav1.GetOptions{})
	} else {
		inst, err = ri.Get(ctx, req.Name, metav1.GetOptions{})
	}
	if apierrors.IsNotFound(err) {
		log.Info("instance not found (likely deleted)")
		return nil
	}
	if err != nil {
		log.Error(err, "failed loading instance")
		return err
	}

	// Emit condition events on every return path (behind feature gate).
	var rcx *ReconcileContext
	if features.FeatureGate.Enabled(features.InstanceConditionEvents) {
		initialConditions := conditionsFromInstance(inst)
		defer func() {
			obj := inst
			if rcx != nil {
				obj = rcx.Instance
			}
			finalConditions := conditionsFromInstance(obj)
			emitConditionEvents(c.eventRecorder, obj, initialConditions, finalConditions)
		}()
	}

	//--------------------------------------------------------------
	// 2. Create a fresh runtime for this reconciliation
	//--------------------------------------------------------------
	compiledGraph, err := c.resolveCompiledGraph()
	if err != nil {
		log.Error(err, "failed to resolve graph revision")
		mark := NewConditionsMarkerFor(inst)
		mark.GraphResolutionFailed("%v", err)
		if statusErr := c.updateConditionsStatus(ctx, inst); statusErr != nil {
			log.Error(statusErr, "failed to update conditions status after graph resolution failure")
		}
		return err
	}
	runtimeObj, err := runtime.FromGraph(compiledGraph, inst, c.reconcileConfig.RGDConfig)
	if err != nil {
		log.Error(err, "failed to create runtime")
		return err
	}

	//--------------------------------------------------------------
	// 3. Build reconciliation context (clients, mapper, labeler, runtime)
	//--------------------------------------------------------------
	rcx = NewReconcileContext(
		ctx, log, c.gvr,
		c.namespaced,
		c.client.Dynamic(),
		c.client.RESTMapper(),
		c.childResourceLabeler,
		runtimeObj,
		c.reconcileConfig,
		inst,
	)
	rcx.Watcher = watcher

	//--------------------------------------------------------------
	// 4. Handle deletion: clean up children and status
	//--------------------------------------------------------------
	if inst.GetDeletionTimestamp() != nil {
		if err := c.reconcileDeletion(rcx); err != nil {
			_ = c.updateStatus(rcx)
			return err
		}
		return c.updateStatus(rcx)
	}

	//--------------------------------------------------------------
	// 5. Ensure finalizer + management labels before mutating children
	//--------------------------------------------------------------
	if err := c.ensureManaged(rcx); err != nil {
		rcx.Mark.InstanceNotManaged("finalizer/labeling failed: %v", err)
		_ = c.updateStatus(rcx)
		return err
	}

	//--------------------------------------------------------------
	// 6. Resolve Graph (CEL, dependencies); allow data-pending
	//--------------------------------------------------------------
	rcx.Mark.GraphResolved()

	//--------------------------------------------------------------
	// 7. Reconcile nodes (SSA + prune) and update runtime state, only if the suspend annotation is not present.
	//--------------------------------------------------------------
	annotations := inst.GetAnnotations()
	reconcileState := annotations[v1alpha1.InstanceReconcileAnnotation]
	if !v1alpha1.IsReconcileSuspended(reconcileState) {
		if err := c.reconcileNodes(rcx); err != nil {
			if deletingErr, ok2 := errors.AsType[*resourceDeletingError](err); ok2 {
				rcx.Mark.ResourcesDeleting("%v", deletingErr)
				_ = c.updateStatus(rcx)
				return rcx.delayedRequeue(deletingErr)
			}
			rcx.Mark.ResourcesNotReady("resource reconciliation failed: %v", err)
			_ = c.updateStatus(rcx)
			return err
		}
	}
	// Only mark ResourcesReady if all resources reached terminal state.
	// Resources with unsatisfied readyWhen are in WaitingForReadiness,
	// which keeps StateManager.State as IN_PROGRESS.
	switch rcx.StateManager.State {
	case v1alpha1.InstanceStateActive:
		rcx.Mark.ResourcesReady()
	case v1alpha1.InstanceStateError:
		if err := rcx.StateManager.NodeErrors(); err != nil {
			rcx.Mark.ResourcesNotReady("resource error: %v", err)
		} else {
			rcx.Mark.ResourcesNotReady("resource reconciliation error")
		}
	case v1alpha1.InstanceStateInProgress:
		err := rcx.StateManager.NodeErrors()
		rcx.Mark.ResourcesNotReady("awaiting resource readiness: %v", err)
	default:
		rcx.Mark.ResourcesNotReady("unknown instance state")
	}

	//--------------------------------------------------------------
	// 8. Persist status/conditions
	//--------------------------------------------------------------
	return c.updateStatus(rcx)
}

func (c *Controller) ensureManaged(rcx *ReconcileContext) error {
	patched, err := c.applyManagedFinalizerAndLabels(rcx)
	if err != nil {
		return err
	}
	if patched != nil {
		rcx.Instance = patched
		rcx.Runtime.Instance().SetObserved([]*unstructured.Unstructured{patched})
		rcx.Mark = NewConditionsMarkerFor(rcx.Instance)
	}
	rcx.Mark.InstanceManaged()
	return nil
}

const (
	graphResolutionReasonNotAvailable = "not_available"
	graphResolutionReasonFailed       = "failed"
	graphResolutionReasonUnknown      = "unknown_state"
)

func (c *Controller) resolveCompiledGraph() (*graph.Graph, error) {
	gvr := c.gvr.String()
	latest, ok := c.graphResolver.GetLatestRevision()
	if !ok {
		metrics.InstanceGraphResolutionFailuresTotal.WithLabelValues(gvr, graphResolutionReasonNotAvailable).Inc()
		return nil, requeue.NeededAfter(fmt.Errorf("latest issued graph revision not available"), c.reconcileConfig.DefaultRequeueDuration)
	}

	switch latest.State {
	case revisions.RevisionStateActive:
		// Active implies the newest issued revision compiled successfully and its
		// graph is present; this is guaranteed by the GR reconciler and registry
		// invariant.
		metrics.InstanceGraphResolutionSuccessTotal.WithLabelValues(gvr).Inc()
		return latest.CompiledGraph, nil
	case revisions.RevisionStatePending:
		metrics.InstanceGraphResolutionPendingTotal.WithLabelValues(gvr).Inc()
		return nil, requeue.NeededAfter(
			fmt.Errorf("latest issued graph revision %d is pending", latest.Revision),
			c.reconcileConfig.DefaultRequeueDuration,
		)
	case revisions.RevisionStateFailed:
		metrics.InstanceGraphResolutionFailuresTotal.WithLabelValues(gvr, graphResolutionReasonFailed).Inc()
		return nil, requeue.None(fmt.Errorf("latest issued graph revision %d failed", latest.Revision))
	default:
		metrics.InstanceGraphResolutionFailuresTotal.WithLabelValues(gvr, graphResolutionReasonUnknown).Inc()
		return nil, requeue.NeededAfter(
			fmt.Errorf("latest issued graph revision %d has unknown state %q", latest.Revision, latest.State),
			c.reconcileConfig.DefaultRequeueDuration,
		)
	}
}

func (c *Controller) applyManagedFinalizerAndLabels(rcx *ReconcileContext) (*unstructured.Unstructured, error) {
	obj := rcx.Instance
	// Fast path: if everything is already correct → no patch
	hasFinalizer := metadata.HasInstanceFinalizer(obj)
	needFinalizer := !hasFinalizer

	wantLabels := c.instanceLabeler.Labels()
	haveLabels := obj.GetLabels()
	needLabelPatch := false

	for k, v := range wantLabels {
		if haveLabels[k] != v {
			needLabelPatch = true
			break
		}
	}

	if needPatch := needFinalizer || needLabelPatch; !needPatch {
		return obj, nil
	}

	//-------------------------------------------
	// Build a minimal patch object (SSA apply)
	//-------------------------------------------
	patch := instanceSSAPatch(obj)

	// Label + finalizers patch
	// we patch together here because otherwise we could revert a previous patch
	// result if only one of finalizers or labels change.
	patch.SetLabels(maps.Clone(wantLabels))
	metadata.SetInstanceFinalizer(patch)

	patched, err := rcx.InstanceClient().Apply(
		rcx.Ctx,
		obj.GetName(),
		patch,
		metav1.ApplyOptions{
			FieldManager: FieldManagerForLabeler,
			Force:        true,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed applying managed finalizer/labels: %w", err)
	}

	return patched, nil
}
