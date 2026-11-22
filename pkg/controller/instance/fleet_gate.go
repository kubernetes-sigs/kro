// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package instance

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/cel-go/cel"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/library"
	"github.com/kubernetes-sigs/kro/pkg/features"
	"github.com/kubernetes-sigs/kro/pkg/graph/parser"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
)

// POC for KREP-026 use case 5: cross-instance rollout of a new GraphRevision.
//
// The RGD's spec.propagateWhen is evaluated per instance when a newer revision
// exists than status.observedRevision. Scope:
//
//	instances  list of every instance of this definition, each a map carrying
//	           metadata/spec plus the reserved ready()/updated() facts
//	each       the instance under decision
//
// Two persisted revision numbers: observedRevision is the revision the
// instance was admitted to (claimed before the apply); convergedRevision is
// the last revision it reached Ready=True on. updated() = observedRevision ==
// latest. ready() = convergedRevision == observedRevision. The Ready condition
// itself is not a gate input: a held instance is frozen and kro does not know
// its current health. An instance with observed != converged is in flight on
// its revision and is never gated.

const (
	fleetVarName = "instances"
	eachVarName  = "each"
	// observedRevisionField records the revision the instance was admitted to.
	observedRevisionField = "observedRevision"
	// convergedRevisionField records the last revision the instance reached
	// Ready=True on.
	convergedRevisionField = "convergedRevision"
)

// admissionLedger serializes fleet-gate decisions for one definition and
// bridges the gap between "this instance was admitted to the latest revision"
// and "the informer/LIST shows its observedRevision write". Workers evaluate
// the gate under mu, so each evaluation sees every admission made before it.
// Entries are released when the snapshot confirms the write, when the
// admitting reconcile fails, or after admissionGrace.
type admissionLedger struct {
	mu       sync.Mutex
	revision int64
	admitted map[string]time.Time
	// held records instances currently held by the gate so a converging
	// sibling can wake them instead of waiting for the poll interval.
	held map[types.NamespacedName]struct{}
}

func (l *admissionLedger) markHeld(k types.NamespacedName) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held == nil {
		l.held = map[types.NamespacedName]struct{}{}
	}
	l.held[k] = struct{}{}
}

// takeHeld removes and returns up to n held instances to wake. A converging
// instance frees one slot, so waking everything held would make every
// convergence cost O(held) gate evaluations, each an O(N) snapshot: O(N^2)
// per convergence, O(N^3) per rollout. Waking n keeps it O(n*N). Anything not
// woken is picked up by its periodic requeue.
func (l *admissionLedger) takeHeld(n int) []types.NamespacedName {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]types.NamespacedName, 0, min(n, len(l.held)))
	for k := range l.held {
		if len(out) == n {
			break
		}
		out = append(out, k)
		delete(l.held, k)
	}
	return out
}

// admissionGrace bounds how long an admission is counted without its
// observedRevision claim becoming visible. The claim is written immediately
// after admission, so this only covers a claim write in progress (or a
// controller that died between admit and claim); it is not a reconcile
// timeout.
const admissionGrace = 30 * time.Second

// resetIfRevisionChanged re-keys the ledger to a new latest revision. Entries
// are KEPT: an instance admitted to r2 whose claim has not landed is still
// disrupted when r3 publishes, and it will take r3 ungated on its next
// reconcile. Wiping it here let two publishes within admissionGrace admit two
// caps' worth. Entries still age out by admissionGrace or reap when visible.
func (l *admissionLedger) resetIfRevisionChanged(rev int64) {
	l.revision = rev
	if l.admitted == nil {
		l.admitted = map[string]time.Time{}
	}
}

// release drops an admission (reconcile failed before the write landed).
func (l *admissionLedger) release(rev int64, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.revision == rev {
		delete(l.admitted, key)
	}
}

// claimRevision persists the admission before any apply: status.observedRevision
// = rev, plus the informational RevisionReconciled=False/Updating. Advancing
// observedRevision while convergedRevision stays behind is what makes the
// instance in flight (see inFlight), so from the instant the claim is visible
// it counts as updated() && !ready() in every snapshot; the in-memory ledger
// only has to cover the write's latency.
func (c *Controller) claimRevision(ctx context.Context, inst *unstructured.Unstructured, rev int64) error {
	return c.replaceRevisionCondition(ctx, inst, map[string]any{
		"type":               RevisionReconciled,
		"status":             string(metav1.ConditionFalse),
		"reason":             claimReason,
		"message":            fmt.Sprintf("admitted to revision %d, reconcile in progress", rev),
		"observedGeneration": inst.GetGeneration(),
	}, map[string]any{observedRevisionField: rev})
}

func instanceKeyOf(inst *unstructured.Unstructured) string {
	return inst.GetNamespace() + "/" + inst.GetName()
}

func (c *Controller) compileFleetGate() ([]*krocel.Expression, error) {
	c.fleetGateOnce.Do(func() {
		if len(c.reconcileConfig.FleetGate) == 0 {
			return
		}
		if !features.FeatureGate.Enabled(features.PropagateWhen) {
			c.fleetGateErr = fmt.Errorf("spec.propagateWhen requires the PropagateWhen feature gate to be enabled")
			return
		}
		env, err := krocel.DefaultEnvironment(krocel.WithCustomDeclarations([]cel.EnvOption{
			cel.Variable(fleetVarName, cel.ListType(cel.DynType)),
			cel.Variable(eachVarName, cel.DynType),
		}))
		if err != nil {
			c.fleetGateErr = fmt.Errorf("fleet gate env: %w", err)
			return
		}
		exprs, err := parser.UnwrapExpressions(c.reconcileConfig.FleetGate)
		if err != nil {
			c.fleetGateErr = fmt.Errorf("fleet gate parse: %w", err)
			return
		}
		factRoots := map[string]struct{}{fleetVarName: {}, eachVarName: {}}
		for _, e := range exprs {
			if err := compiler.CheckItemFactReceivers(env, e.Original, factRoots); err != nil {
				c.fleetGateErr = fmt.Errorf("fleet gate %q: %w", e.Original, err)
				return
			}
			ast, iss := env.Compile(e.Original)
			if iss != nil && iss.Err() != nil {
				c.fleetGateErr = fmt.Errorf("fleet gate %q: %w", e.Original, iss.Err())
				return
			}
			prog, err := env.Program(ast, krocel.ProgramOptions(c.reconcileConfig.CELCostLimit)...)
			if err != nil {
				c.fleetGateErr = fmt.Errorf("fleet gate %q: %w", e.Original, err)
				return
			}
			e.Program = prog
		}
		c.fleetGateExprs = exprs
	})
	return c.fleetGateExprs, c.fleetGateErr
}

// observedRevision reads status.observedRevision; 0 means never recorded.
func observedRevision(inst *unstructured.Unstructured) int64 {
	v, found, err := unstructured.NestedInt64(inst.Object, "status", observedRevisionField)
	if !found || err != nil {
		// Some decoders surface JSON numbers as float64.
		if f, ok, _ := unstructured.NestedFloat64(inst.Object, "status", observedRevisionField); ok {
			return int64(f)
		}
		return 0
	}
	return v
}

// claimReason is the RevisionReconciled reason written by claimRevision.
// Informational: ready() does not read it.
const claimReason = "Updating"

// convergedRevision reads status.convergedRevision; 0 means never converged.
func convergedRevision(inst *unstructured.Unstructured) int64 {
	v, found, err := unstructured.NestedInt64(inst.Object, "status", convergedRevisionField)
	if !found || err != nil {
		if f, ok, _ := unstructured.NestedFloat64(inst.Object, "status", convergedRevisionField); ok {
			return int64(f)
		}
		return 0
	}
	return v
}

// instanceConverged reports whether the instance has converged on the revision
// it was admitted to. convergedRevision is always persisted alongside
// observedRevision (0 until the first Ready reconcile), so an instance that has
// never converged is in flight, not converged. An instance from before this
// feature has neither field: observedRevision == 0 and it skips the gate on
// its first reconcile, which writes both.
func instanceConverged(inst *unstructured.Unstructured) bool {
	return observedRevision(inst) == convergedRevision(inst)
}

// inFlight reports whether the instance was admitted to a revision it has not
// yet converged on. Such an instance is already disrupted and is never gated.
func inFlight(inst *unstructured.Unstructured) bool {
	return !instanceConverged(inst)
}

// instanceFacts projects one instance into the gate's scope value.
func instanceFacts(inst *unstructured.Unstructured, latest int64) map[string]any {
	// Top-level copy only: the gate reads, never writes, and the object is
	// discarded after one evaluation. A deep copy of every instance on every
	// evaluation was the dominant per-evaluation cost.
	// Project metadata and spec only. status is frozen for a held instance,
	// so a gate reading i.status.* would be reading stale data and could pin
	// itself (a gate on the Ready condition deadlocked in review). The facts
	// below are the only status-derived inputs, and kro keeps them current.
	m := map[string]any{
		"apiVersion": inst.Object["apiVersion"],
		"kind":       inst.Object["kind"],
		"metadata":   inst.Object["metadata"],
		"spec":       inst.Object["spec"],
	}
	// updated(): on the latest revision, or in flight on any revision. An
	// in-flight instance takes the latest revision ungated on its next
	// reconcile, so for the budget it is already "work on the latest
	// revision"; counting it as !updated() between a publish and that
	// reconcile under-counted the budget and doubled the disrupted set.
	m[library.ItemUpdatedKey] = observedRevision(inst) == latest || inFlight(inst)
	// ready(): converged on the revision it was admitted to. The Ready
	// condition is NOT consulted: a held instance is frozen, so kro does not
	// know its current health, and a fact kro cannot keep current must not
	// drive admission (it leaked in one direction and deadlocked in the other
	// during review). On the fleet, !ready() and updated() && !ready() are
	// the same set.
	m[library.ItemReadyKey] = instanceConverged(inst)
	return m
}

// selectRevision decides which revision this reconcile runs against. With no
// gate, or no prior observed revision, or already on latest: latest. Otherwise
// the gate is evaluated against the fleet; held → the instance's observed
// revision (if still resolvable), with the failing expression returned.
// The returned bool is true only when the gate was evaluated and admitted
// this instance (an entry was recorded in the ledger); the caller must then
// claim the revision or release the slot.
func (c *Controller) selectRevision(ctx context.Context, inst *unstructured.Unstructured, latest revisions.Entry) (revisions.Entry, string, bool, error) {
	exprs, err := c.compileFleetGate()
	if err != nil || len(exprs) == 0 {
		return latest, "", false, err
	}
	observed := observedRevision(inst)
	if observed == 0 || observed >= latest.Revision {
		return latest, "", false, nil
	}
	// An instance admitted to a revision it has not converged on is already
	// disrupted. Holding it protects nothing, and because a held instance is
	// frozen its readiness could never change, so a gate that counts
	// !ready() would be stuck on it forever. It takes the latest revision
	// without being gated and counts as in flight there. It is still recorded
	// in the ledger so concurrent workers count it before its claim lands.
	//
	// Keyed on observed != converged, NOT on Ready=False: a converged
	// instance that blipped unready (node drain, HPA) is gated and counted
	// like any other, which is the correlated-event case the cap exists for.
	if inFlight(inst) {
		c.ledger.mu.Lock()
		c.ledger.resetIfRevisionChanged(latest.Revision)
		c.ledger.admitted[instanceKeyOf(inst)] = time.Now()
		c.ledger.mu.Unlock()
		return latest, "", true, nil
	}

	// The fleet is every instance of this RGD in every namespace. A real
	// implementation reads the parent informer; the POC lists.
	list, err := c.client.Dynamic().Resource(c.gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return latest, "", false, fmt.Errorf("fleet gate: list instances: %w", err)
	}

	// Build the snapshot OUTSIDE the lock: deep copies and the sort are the
	// expensive part and depend on nothing shared.
	//
	// Deterministic order: namespace/name. Authors sort in CEL if they need
	// something else.
	sort.Slice(list.Items, func(i, j int) bool {
		if list.Items[i].GetNamespace() != list.Items[j].GetNamespace() {
			return list.Items[i].GetNamespace() < list.Items[j].GetNamespace()
		}
		return list.Items[i].GetName() < list.Items[j].GetName()
	})
	fleet := make([]any, 0, len(list.Items))
	facts := make([]map[string]any, len(list.Items))
	visible := map[string]bool{}
	var me map[string]any
	for i := range list.Items {
		f := instanceFacts(&list.Items[i], latest.Revision)
		facts[i] = f
		fleet = append(fleet, f)
		if list.Items[i].GetUID() == inst.GetUID() {
			me = f
		}
		if observedRevision(&list.Items[i]) == latest.Revision {
			visible[instanceKeyOf(&list.Items[i])] = true
		}
	}
	if me == nil {
		me = instanceFacts(inst, latest.Revision)
	}
	scope := map[string]any{fleetVarName: fleet, eachVarName: me}

	// Critical section: reap, overlay, evaluate, admit. Concurrent workers
	// see each other's admissions here before the claim writes are visible
	// in any snapshot.
	//
	// Revision rollover: the ledger is keyed by revision and wiped when it
	// changes. Instances still converging on an older revision are in flight
	// (observed != converged) and take the new revision ungated above, so
	// they are counted on the new revision; the disrupted set does not grow.
	c.ledger.mu.Lock()
	defer c.ledger.mu.Unlock()
	c.ledger.resetIfRevisionChanged(latest.Revision)
	now := time.Now()
	for k, t := range c.ledger.admitted {
		if visible[k] || now.Sub(t) > admissionGrace {
			delete(c.ledger.admitted, k)
		}
	}
	for i := range list.Items {
		if _, ok := c.ledger.admitted[instanceKeyOf(&list.Items[i])]; ok {
			facts[i][library.ItemUpdatedKey] = true
			facts[i][library.ItemReadyKey] = false
		}
	}

	for _, e := range exprs {
		v, err := e.Eval(scope)
		if err != nil {
			return latest, "", false, fmt.Errorf("fleet gate %q: %w", e.Original, err)
		}
		b, ok := v.(bool)
		if !ok {
			return latest, "", false, fmt.Errorf("fleet gate %q returned %T, want bool", e.Original, v)
		}
		if !b {
			// Held. The caller freezes the instance and applies nothing, so the
			// revision it is on does not need to be resolvable.
			return latest, e.Original, false, nil
		}
	}
	// Admitted: record it before releasing the lock so the next evaluation
	// counts this instance as in flight.
	c.ledger.admitted[instanceKeyOf(inst)] = now
	return latest, "", true, nil
}

// heldMessage is the RevisionReconciled message for a held instance.
func heldMessage(on, latest int64, expr string) string {
	return fmt.Sprintf("held on revision %d, latest is %d: propagateWhen %q is false", on, latest, expr)
}

// heldConditionCurrent reports whether the instance already carries the exact
// Held condition this evaluation would write.
func heldConditionCurrent(inst *unstructured.Unstructured, on, latest int64, expr string) bool {
	conds, _, _ := unstructured.NestedSlice(inst.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != RevisionReconciled {
			continue
		}
		return m["status"] == string(metav1.ConditionFalse) && m["reason"] == "Held" && m["message"] == heldMessage(on, latest, expr)
	}
	return false
}

// writeHeldCondition replaces RevisionReconciled with Held via a merge patch
// on the status subresource. Nothing else in status is touched: the
// instance's state and other conditions describe the revision it is frozen
// on and remain true.
func (c *Controller) writeHeldCondition(ctx context.Context, inst *unstructured.Unstructured, on, latest int64, expr string) error {
	return c.replaceRevisionCondition(ctx, inst, map[string]any{
		"type":               RevisionReconciled,
		"status":             string(metav1.ConditionFalse),
		"reason":             "Held",
		"message":            heldMessage(on, latest, expr),
		"observedGeneration": inst.GetGeneration(),
	}, nil)
}

// replaceRevisionCondition reads the instance fresh, replaces its
// RevisionReconciled condition with cond (preserving lastTransitionTime when
// the status did not change), merges extra status fields, and writes with a
// resourceVersion precondition so a stale cached copy can never write back a
// stale Ready. The fresh read is what makes the whole-list merge patch safe.
func (c *Controller) replaceRevisionCondition(ctx context.Context, inst *unstructured.Unstructured, cond map[string]any, extra map[string]any) error {
	cur, err := c.instanceClient(inst).Get(ctx, inst.GetName(), metav1.GetOptions{})
	if err != nil {
		return err
	}
	conds, _, _ := unstructured.NestedSlice(cur.Object, "status", "conditions")
	merged := make([]any, 0, len(conds)+1)
	ltt := metav1.Now().UTC().Format(time.RFC3339)
	for _, cnd := range conds {
		m, ok := cnd.(map[string]any)
		if ok && m["type"] == RevisionReconciled {
			if m["status"] == cond["status"] {
				if prev, ok := m["lastTransitionTime"].(string); ok && prev != "" {
					ltt = prev
				}
			}
			continue
		}
		merged = append(merged, cnd)
	}
	cond["lastTransitionTime"] = ltt
	merged = append(merged, cond)
	status := map[string]any{"conditions": merged}
	for k, v := range extra {
		status[k] = v
	}
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": cur.GetResourceVersion()},
		"status":   status,
	})
	if err != nil {
		return err
	}
	if _, err := c.instanceClient(inst).Patch(ctx, inst.GetName(), types.MergePatchType, body, metav1.PatchOptions{}, "status"); err != nil {
		return err
	}
	// Mirror onto the in-memory object so later code in this reconcile reads
	// what is on the wire.
	if inst.Object["status"] == nil {
		inst.Object["status"] = map[string]any{}
	}
	if st, ok := inst.Object["status"].(map[string]any); ok {
		for k, v := range status {
			st[k] = v
		}
	}
	return nil
}
