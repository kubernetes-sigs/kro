package instance

import (
	"fmt"

	"github.com/kubernetes-sigs/kro/pkg/applyset"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

func (c *Controller) reconcileResources(rcx *ReconcileContext) error {
	rcx.Log.V(2).Info("Reconciling resources")

	order := rcx.Runtime.TopologicalOrder()

	// ---------------------------------------------------------
	// 1. Prepare ApplySet
	// ---------------------------------------------------------
	applySet, err := c.createApplySet(rcx)
	if err != nil {
		return rcx.delayedRequeue(fmt.Errorf("applyset setup failed: %w", err))
	}

	// Values updated during the loop
	var unresolved string
	prune := true

	// ---------------------------------------------------------
	// 2. Reconcile each resource in topological order
	// ---------------------------------------------------------
	for _, id := range order {
		if err := c.reconcileResource(rcx, applySet, id, &unresolved, &prune); err != nil {
			return err
		}
	}

	// ---------------------------------------------------------
	// 3. Apply all accumulated changes
	// ---------------------------------------------------------
	result, err := applySet.Apply(rcx.Ctx, prune)
	if err != nil {
		return rcx.delayedRequeue(fmt.Errorf("apply/prune failed: %w", err))
	}

	// ---------------------------------------------------------
	// 4. Process results and update runtime state
	// ---------------------------------------------------------
	if err := c.processApplyResults(rcx, result); err != nil {
		return rcx.delayedRequeue(err)
	}

	// ---------------------------------------------------------
	// 5. Final resolution checks
	// ---------------------------------------------------------
	if unresolved != "" {
		return rcx.delayedRequeue(fmt.Errorf("waiting for unresolved resource %q", unresolved))
	}
	if result.HasClusterMutation() {
		/* We must requeue after cluster mutation so CEL values re-evaluate. */
		return rcx.delayedRequeue(fmt.Errorf("cluster mutated"))
	}

	return nil
}

func (c *Controller) reconcileResource(
	rcx *ReconcileContext,
	aset applyset.Set,
	id string,
	unresolved *string,
	prune *bool,
) error {
	rcx.Log.V(3).Info("Reconciling resource", "id", id)

	st := &ResourceState{State: ResourceStateInProgress}
	rcx.StateManager.ResourceStates[id] = st

	// 1. Should we process?
	want, err := rcx.Runtime.ReadyToProcessResource(id)
	if err != nil || !want {
		st.State = ResourceStateSkipped
		rcx.Log.V(2).Info("Skipping resource", "id", id, "reason", err)
		rcx.Runtime.IgnoreResource(id)
		return nil
	}

	// 2. Must be resolved
	_, rstate := rcx.Runtime.GetResource(id)
	if rstate != runtime.ResourceStateResolved {
		*unresolved = id
		*prune = false
		return nil
	}

	// 3. Dependencies must be ready
	if !rcx.Runtime.AreDependenciesReady(id) {
		*unresolved = id
		*prune = false
		return nil
	}

	// 4. External reference
	if rcx.Runtime.ResourceDescriptor(id).IsExternalRef() {
		return c.handleExternalRef(rcx, id, st)
	}

	// 5. Regular resource via ApplySet
	return c.handleApplySetResource(rcx, aset, id, st)
}

func (c *Controller) createApplySet(rcx *ReconcileContext) (applyset.Set, error) {
	lbl, err := metadata.NewInstanceLabeler(rcx.Runtime.GetInstance()).
		Merge(rcx.Labeler)
	if err != nil {
		return nil, fmt.Errorf("labeler merge: %w", err)
	}

	cfg := applyset.Config{
		ToolLabels:   lbl.Labels(),
		FieldManager: FieldManagerForApplyset,
		ToolingID:    KROTooling,
		Log:          rcx.Log,
	}

	return applyset.New(rcx.Runtime.GetInstance(), rcx.RestMapper, rcx.Client, cfg)
}

func (c *Controller) handleApplySetResource(
	rcx *ReconcileContext,
	aset applyset.Set,
	id string,
	st *ResourceState,
) error {
	desired, _ := rcx.Runtime.GetResource(id)

	applyable := applyset.ApplyableObject{
		Unstructured: desired,
		ID:           id,
	}

	actual, err := aset.Add(rcx.Ctx, applyable)
	if err != nil {
		st.State = ResourceStateError
		st.Err = err
		return err
	}

	if actual != nil {
		rcx.Runtime.SetResource(id, actual)
		updateReadiness(rcx, id)
		_, err := rcx.Runtime.Synchronize()
		return err
	}
	return nil
}

func updateReadiness(rcx *ReconcileContext, id string) {
	st := rcx.StateManager.ResourceStates[id]

	ready, reason, err := rcx.Runtime.IsResourceReady(id)
	if err != nil || !ready {
		st.State = ResourceStateWaitingForReadiness
		st.Err = fmt.Errorf("not ready: %s: %w", reason, err)
	} else {
		st.State = ResourceStateSynced
	}
}

func (c *Controller) handleExternalRef(
	rcx *ReconcileContext,
	id string,
	st *ResourceState,
) error {
	desired, _ := rcx.Runtime.GetResource(id)

	actual, err := c.readExternalRef(rcx, id, desired)
	if err != nil {
		st.State = ResourceStateError
		st.Err = err
		return nil
	}

	rcx.Runtime.SetResource(id, actual)
	updateReadiness(rcx, id)
	_, err = rcx.Runtime.Synchronize()
	return err
}

func (c *Controller) readExternalRef(
	rcx *ReconcileContext,
	resourceID string,
	desired *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {

	gvk := desired.GroupVersionKind()

	// 1. Map GVK → GVR
	mapping, err := c.client.RESTMapper().RESTMapping(
		gvk.GroupKind(),
		gvk.Version,
	)
	if err != nil {
		return nil, fmt.Errorf("externalRef: RESTMapping for %s: %w", gvk, err)
	}

	// 2. Determine which client to use
	var ri dynamic.ResourceInterface

	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := desired.GetNamespace()
		if ns == "" {
			ns = rcx.getResourceNamespace(resourceID)
		}
		ri = c.client.Dynamic().Resource(mapping.Resource).Namespace(ns)
	} else {
		ri = c.client.Dynamic().Resource(mapping.Resource)
	}

	// 3. Fetch existing object
	name := desired.GetName()

	obj, err := ri.Get(rcx.Ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("externalRef: GET %s %s/%s: %w",
			gvk.String(), desired.GetNamespace(), name, err,
		)
	}

	rcx.Log.Info("External reference resolved",
		"resourceID", resourceID,
		"gvk", gvk.String(),
		"namespace", obj.GetNamespace(),
		"name", obj.GetName(),
	)

	return obj, nil
}

func (c *Controller) processApplyResults(
	rcx *ReconcileContext,
	result *applyset.ApplyResult,
) error {

	rcx.Log.V(2).Info("Processing apply results")

	for _, r := range result.AppliedObjects {
		st := rcx.StateManager.ResourceStates[r.ID]

		// ---------------------------------------------------------
		// 1. Handle apply error for this specific resource
		// ---------------------------------------------------------
		if r.Error != nil {
			st.State = ResourceStateError
			st.Err = r.Error
			rcx.Log.V(1).Info("apply error", "id", r.ID, "error", r.Error)
			continue
		}

		// ---------------------------------------------------------
		// 2. Update runtime with the new applied object
		// ---------------------------------------------------------
		if r.LastApplied != nil {
			rcx.Runtime.SetResource(r.ID, r.LastApplied)
			updateReadiness(rcx, r.ID)

			if _, err := rcx.Runtime.Synchronize(); err != nil {
				st.State = ResourceStateError
				st.Err = fmt.Errorf("failed to synchronize after apply: %w", err)
				continue
			}
		}
	}

	// ---------------------------------------------------------
	// 3. Aggregate all resource errors
	// ---------------------------------------------------------
	if err := rcx.StateManager.ResourceErrors(); err != nil {
		return fmt.Errorf("apply results contain errors: %w", err)
	}

	return nil
}
