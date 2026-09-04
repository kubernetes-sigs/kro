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
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	kroclient "github.com/kubernetes-sigs/kro/pkg/client"
	"github.com/kubernetes-sigs/kro/pkg/controller/backoff"
	"github.com/kubernetes-sigs/kro/pkg/controller/instance/applyset"
	"github.com/kubernetes-sigs/kro/pkg/dynamiccontroller"
	"github.com/kubernetes-sigs/kro/pkg/features"

	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/rgdadapter"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/metrics"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
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
	// HasAuthorConditions is true when the RGD declares an author
	// `conditions:` block, which replaces kro's built-in conditions on
	// .status.conditions[].
	HasAuthorConditions bool
	// MaxCollectionSize is the maximum number of instances a single
	// forEach collection expansion may generate.
	MaxCollectionSize int
	// ApplyConcurrency bounds the number of concurrent SSA apply operations
	// executed in parallel for collection nodes. 0 means use default (20).
	ApplyConcurrency int
	// CELCostLimit bounds CEL evaluation cost for author status/condition
	// projection (0 = disabled). Threaded into ProjectInstanceConditions so
	// author conditions share the same execution bound as graph expressions
	// instead of silently using DefaultProgramOptions (unbounded).
	CELCostLimit uint64
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

	graphResolver revisions.Resolver
	namespaced    bool

	instanceLabeler metadata.Labeler
	reconcileConfig ReconcileConfig
	coordinator     *dynamiccontroller.WatchCoordinator

	// eventRecorder emits K8s Events on condition transitions.
	eventRecorder record.EventRecorder

	// graphEngineCompiler and graphEngineExecutor drive instance reconciliation.
	// The executor is wired at construction; the compiler is injected via
	// WithGraphEngineCompiler after SetupWithManager.
	graphEngineCompiler rgdadapter.Compiler
	graphEngineExecutor *executor.Simple

	// programCache reuses one compiled Program across every instance of a
	// revision, keyed by (owner, spec-hash), so BuildRuntimeForInstanceCached
	// skips the per-reconcile compile. Owned by this controller (one per RGD),
	// so it holds at most a couple of entries and is discarded with it.
	programCache *registry.Registry

	// feature gate flags, captured once at construction time.
	eventsEnabled  bool
	metricsEnabled bool

	// backoff tracks per-instance consecutive soft not-ready attempts so the
	// requeue delay grows (capped) instead of polling a never-resolving
	// reference at a flat interval forever. Lazily initialized via backoffOnce
	// so a directly-constructed Controller (tests) works too. Its first-attempt
	// delay is seeded from reconcileConfig.DefaultRequeueDuration.
	backoff     *backoff.Tracker
	backoffOnce sync.Once
}

// ensureBackoff lazily initializes the per-instance requeue backoff tracker,
// seeding its base delay from the configured DefaultRequeueDuration. Safe to
// call from multiple reconcile workers.
func (c *Controller) ensureBackoff() {
	c.backoffOnce.Do(func() {
		if c.backoff == nil {
			c.backoff = backoff.New(c.reconcileConfig.DefaultRequeueDuration)
		}
	})
}

// NewController constructs a new controller that resolves the newest issued
// graph revision for the RGD from a revisions.Resolver.
//
// The caller must supply a non-nil graphEngineClient (a controller-runtime
// client.Client) used by the Graph engine executor.
func NewController(
	log logr.Logger,
	reconcileConfig ReconcileConfig,
	gvr schema.GroupVersionResource,
	graphResolver revisions.Resolver,
	namespaced bool,
	kroClient kroclient.SetInterface,
	instanceLabeler metadata.Labeler,
	childResourceLabeler metadata.Labeler,
	coord *dynamiccontroller.WatchCoordinator,
	eventRecorder record.EventRecorder,
	graphEngineClient client.Client,
) *Controller {
	exec := executor.NewSimple(graphEngineClient)
	exec.ApplyConcurrency = reconcileConfig.ApplyConcurrency
	// Gate dependents on their dependencies' readyWhen so a child is not created
	// until the resources it depends on report ready.
	exec.GateReadiness = true
	if childResourceLabeler != nil {
		lab := childResourceLabeler
		exec.WithLabelInjector(func(obj *unstructured.Unstructured) {
			lab.ApplyLabels(obj)
		})
	}
	// Surface a tolerated collection-update rejection (an already-existing item
	// whose SSA update was rejected — e.g. an immutable field — that we keep-live
	// and converge on) as a Warning Event on the affected CHILD object, so the
	// dropped desired change is visible in `kubectl describe` and not just the
	// controller log. Observational only: this hook never influences readiness or
	// requeue (the node still converges), so it cannot reintroduce the wedge an
	// unfixable update would otherwise cause. Set at construction (not per
	// reconcile) because the executor is shared across concurrent reconcile
	// workers; the event subject is derived from the rejection's own target
	// identity rather than any per-reconcile instance state.
	if eventRecorder != nil {
		exec.OnToleratedRejection = func(r executor.ToleratedRejection) {
			ref := &corev1.ObjectReference{
				APIVersion: r.APIVersion,
				Kind:       r.Kind,
				Namespace:  r.Namespace,
				Name:       r.Name,
			}
			eventRecorder.Eventf(ref, corev1.EventTypeWarning, "UpdateRejected",
				"node %q: desired update rejected (%s) and NOT applied; keeping the live object. Cause: %s",
				r.NodeID, r.Reason, r.Cause)
		}
	}
	// The compiler is set via WithGraphEngineCompiler after construction
	// because it requires rest.Config which the caller owns.

	return &Controller{
		log:                 log,
		client:              kroClient,
		gvr:                 gvr,
		graphResolver:       graphResolver,
		namespaced:          namespaced,
		instanceLabeler:     instanceLabeler,
		reconcileConfig:     reconcileConfig,
		coordinator:         coord,
		eventRecorder:       eventRecorder,
		graphEngineExecutor: exec,
		programCache:        registry.New(),
		eventsEnabled:       features.FeatureGate.Enabled(features.InstanceConditionEvents),
		metricsEnabled:      features.FeatureGate.Enabled(features.InstanceConditionMetrics),
	}
}

// WithGraphEngineCompiler sets the graph-engine compiler on an already-
// constructed Controller.  This two-phase init exists because the compiler
// needs a rest.Config that the instance controller does not own — the RGD
// controller injects it after SetupWithManager.
func (c *Controller) WithGraphEngineCompiler(comp rgdadapter.Compiler) {
	c.graphEngineCompiler = comp
}

// Reconcile implements dynamiccontroller.Handler.
func (c *Controller) Reconcile(ctx context.Context, req ctrl.Request) (err error) {
	c.ensureBackoff()
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
		if c.metricsEnabled {
			metrics.DeleteInstanceMetrics(c.gvr, req.Namespace, req.Name)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed loading instance: %w", err)
	}

	// Snapshot initial conditions and emit telemetry on every return path.
	// Events and metrics are gated behind separate feature flags so operators
	// can enable them independently.
	var dcx *DeletionContext
	if c.eventsEnabled || c.metricsEnabled {
		initialConditions := conditionsFromInstance(inst)
		defer func() {
			obj := inst
			if dcx != nil {
				obj = dcx.Instance
			}
			finalConditions := conditionsFromInstance(obj)
			if c.eventsEnabled {
				emitConditionEvents(c.eventRecorder, obj, initialConditions, finalConditions)
			}
			if c.metricsEnabled {
				metrics.EmitConditionMetrics(log, c.gvr, obj, initialConditions, finalConditions)
			}
		}()
	}

	//--------------------------------------------------------------
	// 2. Handle deletion before graph resolution
	//--------------------------------------------------------------
	// Deletion must not depend on resolving the current GraphRevision or CEL.
	// Build a context without a runtime and use persisted ApplySet inventory.
	if inst.GetDeletionTimestamp() != nil {
		// The instance is going away — end any not-ready backoff streak so a
		// later instance reusing this key starts fresh at the base delay.
		c.backoff.Reset(req.NamespacedName)
		dcx = NewDeletionContext(
			ctx, log, c.gvr, c.namespaced, c.client.Dynamic(), c.client.RESTMapper(),
			c.reconcileConfig, inst,
		)
		if err := c.reconcileDeletion(dcx); err != nil {
			_ = c.updateDeletionStatus(dcx)
			return err
		}
		return c.updateDeletionStatus(dcx)
	}

	//--------------------------------------------------------------
	// 2b. Honor the reconcile-suspended annotation before touching the engine:
	//     mark ResourcesReady=False/ReconciliationSuspended and persist status
	//     without reconciling any nodes.
	//--------------------------------------------------------------
	if v1alpha1.IsReconcileSuspended(inst.GetAnnotations()[v1alpha1.InstanceReconcileAnnotation]) {
		return c.reconcileSuspended(ctx, inst)
	}

	//--------------------------------------------------------------
	// 2c. Reconcile through the Graph engine.
	//--------------------------------------------------------------
	return c.reconcileViaGraphEngine(ctx, inst, watcher)
}

// reconcileSuspended handles an instance carrying the reconcile-suspended
// annotation. The instance stays managed (finalizer + labels are stamped), the
// built-in conditions report InstanceManaged=True, GraphResolved=True and
// ResourcesReady=False with reason "ReconciliationSuspended", and no nodes are
// applied or pruned. Status is persisted so the Reconcile defer emits the
// condition-transition events/metrics.
func (c *Controller) reconcileSuspended(ctx context.Context, inst *unstructured.Unstructured) error {
	// Keep the instance managed even while suspended so deletion still works.
	patched, err := c.stampInstanceMetadata(ctx, inst)
	if err != nil {
		return err
	}
	if patched != nil {
		inst.Object = patched.Object
	}

	wireStatus := captureWireStatus(inst)

	mark := NewConditionsMarkerFor(inst)
	mark.InstanceManaged()
	mark.GraphResolved()
	mark.ReconciliationSuspended("reconciliation suspended via %s annotation", v1alpha1.InstanceReconcileAnnotation)

	// No nodes are reconciled, so the instance-level state is Active (there is
	// nothing to mark not-ready beyond the suspend condition). Author conditions
	// are carried forward from the wire since they cannot be re-evaluated while
	// suspended.
	ri := c.client.Dynamic().Resource(c.gvr)
	var instanceClient dynamic.ResourceInterface = ri
	if c.namespaced {
		instanceClient = ri.Namespace(inst.GetNamespace())
	}
	return c.persistNodeFreeStatus(ctx, instanceClient, inst, wireStatus, v1alpha1.InstanceStateActive)
}

// stampInstanceMetadata stamps the kro finalizer and instance-management labels
// directly via the dynamic client and returns the server's patched object when
// a write was needed (nil when the instance was already correct).
func (c *Controller) stampInstanceMetadata(ctx context.Context, inst *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	// Fast path: nothing to do if labels and finalizer are already present.
	hasFinalizer := metadata.HasInstanceFinalizer(inst)
	needFinalizer := !hasFinalizer
	hasInventoryMetadata := hasAnyApplySetInventoryMetadata(inst)
	if needFinalizer && hasInventoryMetadata {
		if err := applyset.ValidateParentInventory(inst); err != nil {
			return nil, fmt.Errorf(
				"cannot install finalizer with invalid applyset inventory: %w", err)
		}
	}

	wantLabels := c.instanceLabeler.Labels()
	haveLabels := inst.GetLabels()
	needLabelPatch := false
	for k, v := range wantLabels {
		if haveLabels[k] != v {
			needLabelPatch = true
			break
		}
	}

	if !needFinalizer && !needLabelPatch {
		return nil, nil
	}

	patch := instanceSSAPatch(inst)
	patchLabels := maps.Clone(wantLabels)
	if needFinalizer && !hasInventoryMetadata {
		emptyInventory := applyset.Metadata{
			ID:      applyset.ID(inst),
			Tooling: applyset.ToolingID(),
		}
		maps.Copy(patchLabels, emptyInventory.Labels())
		patch.SetAnnotations(emptyInventory.Annotations())
	}
	patch.SetLabels(patchLabels)
	metadata.SetInstanceFinalizer(patch)

	ri := c.client.Dynamic().Resource(c.gvr)
	var instClient dynamic.ResourceInterface
	if c.namespaced {
		instClient = ri.Namespace(inst.GetNamespace())
	} else {
		instClient = ri
	}
	patched, err := instClient.Apply(ctx, inst.GetName(), patch, metav1.ApplyOptions{
		FieldManager: FieldManagerForLabeler,
		Force:        true,
	})
	if err != nil {
		return nil, fmt.Errorf("graph-engine: failed stamping instance metadata: %w", err)
	}
	if patched != nil {
		inst.Object = patched.Object
	}
	return patched, nil
}

var applySetAnnotationKeys = [...]string{
	applyset.ApplySetToolingAnnotation,
	applyset.ApplySetGKsAnnotation,
	applyset.ApplySetAdditionalNamespacesAnnotation,
	applyset.ApplySetInventoryHashAnnotation,
}

// hasAnyApplySetInventoryMetadata deliberately detects partial inventory. If
// any field exists, the caller validates the complete inventory instead of
// replacing it with an empty one, which could hide and orphan managed members.
func hasAnyApplySetInventoryMetadata(obj metav1.Object) bool {
	if _, found := obj.GetLabels()[applyset.ApplySetParentIDLabel]; found {
		return true
	}
	annotations := obj.GetAnnotations()
	for _, key := range applySetAnnotationKeys {
		if _, found := annotations[key]; found {
			return true
		}
	}
	return false
}
