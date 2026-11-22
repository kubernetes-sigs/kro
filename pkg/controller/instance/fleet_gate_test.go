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
	"github.com/kubernetes-sigs/kro/pkg/cel/library"
	"github.com/kubernetes-sigs/kro/pkg/features"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	clientfake "github.com/kubernetes-sigs/kro/pkg/client/fake"
	graphhash "github.com/kubernetes-sigs/kro/pkg/graph/hash"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
)

// fakeResolver serves a fixed latest revision and a set of older active ones.
type fakeResolver struct {
	latest revisions.Entry
	older  map[int64]revisions.Entry
}

func (f fakeResolver) GetLatestRevision() (revisions.Entry, bool) { return f.latest, true }
func (f fakeResolver) GetGraphRevision(rev int64) (revisions.Entry, bool) {
	if rev == f.latest.Revision {
		return f.latest, true
	}
	e, ok := f.older[rev]
	return e, ok
}

func activeEntry(rev int64) revisions.Entry {
	return revisions.Entry{
		OwnerKey: "webapps",
		Revision: rev,
		State:    revisions.RevisionStateActive,
		RGDSpec:  &v1alpha1.ResourceGraphDefinitionSpec{},
	}
}

// fleetInstance builds one WebApp instance at observedRevision rev, Ready or not.
// fleetInstance builds an instance on rev. ready=true: converged on rev.
// ready=false: admitted to rev, still converging (convergedRevision = rev-1).
// For "converged on rev but currently unready" use fleetInstanceBlipped.
func fleetInstance(i int, rev int64, ready bool, tier string) *unstructured.Unstructured {
	conv := rev
	if !ready {
		conv = rev - 1
	}
	return fleetInstanceConv(i, rev, conv, ready, tier)
}

// fleetInstanceBlipped is converged on rev and currently Ready=False: a node
// drain or scale event, not a rollout.
func fleetInstanceBlipped(i int, rev int64, tier string) *unstructured.Unstructured {
	return fleetInstanceConv(i, rev, rev, false, tier)
}

func fleetInstanceConv(i int, rev, conv int64, ready bool, tier string) *unstructured.Unstructured {
	status := string(metav1.ConditionFalse)
	if ready {
		status = string(metav1.ConditionTrue)
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": controllerTestParentGVK.GroupVersion().String(),
		"kind":       controllerTestParentGVK.Kind,
		"metadata": map[string]any{
			"name": fmt.Sprintf("app-%02d", i), "namespace": "default",
			"uid": fmt.Sprintf("uid-%02d", i),
		},
		"spec": map[string]any{"tier": tier},
		"status": map[string]any{
			"observedRevision":  rev,
			"convergedRevision": conv,
			"conditions": []any{map[string]any{
				"type": string(v1alpha1.InstanceConditionTypeReady), "status": status,
			}},
		},
	}}
	return u
}

func gateController(t *testing.T, gate []string, resolver revisions.Resolver, objs ...apimachineryruntime.Object) *Controller {
	t.Helper()
	raw := newControllerTestDynamicClient(t, objs...)
	return &Controller{
		log:             logr.Discard(),
		client:          clientfake.NewFakeSet(raw),
		gvr:             controllerTestParentGVR,
		graphResolver:   resolver,
		namespaced:      true,
		reconcileConfig: ReconcileConfig{FleetGate: gate},
	}
}

// fleetWith returns n instances: the first onNew are on revision 2 (Ready per
// newReady), the rest on revision 1 and Ready.
func fleetWith(n, onNew int, newReady bool) []apimachineryruntime.Object {
	objs := make([]apimachineryruntime.Object, 0, n)
	for i := 0; i < n; i++ {
		if i < onNew {
			objs = append(objs, fleetInstance(i, 2, newReady, "standard"))
		} else {
			objs = append(objs, fleetInstance(i, 1, true, "standard"))
		}
	}
	return objs
}

func pick(objs []apimachineryruntime.Object, i int) *unstructured.Unstructured {
	return objs[i].(*unstructured.Unstructured)
}

const capTen = `${instances.filter(i, i.updated() && !i.ready()).size() < 10}`

// Cap 10: with 10 instances in flight on revision 2 and not Ready, a pending
// instance is held on revision 1. Once 4 of them are Ready, it is admitted.
func TestFleetGate_CapTenHoldsAndReleases(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(2), older: map[int64]revisions.Entry{1: activeEntry(1)}}

	inFlight := fleetWith(30, 10, false)
	c := gateController(t, []string{capTen}, resolver, inFlight...)
	target, heldBy, _, err := c.selectRevision(context.Background(), pick(inFlight, 15), resolver.latest)
	require.NoError(t, err)
	assert.NotEmpty(t, heldBy, "pending instance is held while 10 are in flight")
	assert.Contains(t, heldBy, "instances.filter")

	// 4 of the 10 become Ready → 6 in flight → admitted.
	partly := fleetWith(30, 10, false)
	for i := 0; i < 4; i++ {
		partly[i] = fleetInstance(i, 2, true, "standard")
	}
	c = gateController(t, []string{capTen}, resolver, partly...)
	target, heldBy, _, err = c.selectRevision(context.Background(), pick(partly, 15), resolver.latest)
	require.NoError(t, err)
	assert.Equal(t, int64(2), target.Revision)
	assert.Empty(t, heldBy)
}

// An instance already on the latest revision never evaluates the gate.
func TestFleetGate_AlreadyOnLatestBypasses(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(2), older: map[int64]revisions.Entry{1: activeEntry(1)}}
	objs := fleetWith(30, 10, false)
	c := gateController(t, []string{`${false}`}, resolver, objs...)
	target, heldBy, _, err := c.selectRevision(context.Background(), pick(objs, 3), resolver.latest)
	require.NoError(t, err)
	assert.Equal(t, int64(2), target.Revision, "gate is false but the instance is already current")
	assert.Empty(t, heldBy)
}

// A never-reconciled instance (observedRevision 0) takes the latest: there is
// nothing to roll from.
func TestFleetGate_FreshInstanceTakesLatest(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(2)}
	fresh := fleetInstance(99, 0, false, "standard")
	unstructured.RemoveNestedField(fresh.Object, "status")
	c := gateController(t, []string{`${false}`}, resolver, fresh)
	target, heldBy, _, err := c.selectRevision(context.Background(), fresh, resolver.latest)
	require.NoError(t, err)
	assert.Equal(t, int64(2), target.Revision)
	assert.Empty(t, heldBy)
}

// Canary via each: only tier=canary instances move until every canary is on
// the new revision and Ready.
func TestFleetGate_CanaryFirstViaEach(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(2), older: map[int64]revisions.Entry{1: activeEntry(1)}}
	gate := []string{`${each.spec.tier == 'canary'
	   || instances.filter(i, i.spec.tier == 'canary' && !(i.updated() && i.ready())).size() == 0}`}

	// All on rev 1; app-05 is the canary.
	objs := fleetWith(30, 0, false)
	objs[5] = fleetInstance(5, 1, true, "canary")
	c := gateController(t, gate, resolver, objs...)

	target, _, _, err := c.selectRevision(context.Background(), pick(objs, 5), resolver.latest)
	require.NoError(t, err)
	assert.Equal(t, int64(2), target.Revision, "canary moves first")

	target, heldBy, _, err := c.selectRevision(context.Background(), pick(objs, 12), resolver.latest)
	require.NoError(t, err)
	assert.NotEmpty(t, heldBy, "non-canary held while canary pending")
	assert.NotEmpty(t, heldBy)

	// Canary on rev 2 and Ready → the rest are admitted.
	objs[5] = fleetInstance(5, 2, true, "canary")
	c = gateController(t, gate, resolver, objs...)
	target, _, _, err = c.selectRevision(context.Background(), pick(objs, 12), resolver.latest)
	require.NoError(t, err)
	assert.Equal(t, int64(2), target.Revision)
}

// Bad release: the 10 on revision 2 never go Ready; everyone else stays held.
func TestFleetGate_BadReleasePinsAtCap(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(2), older: map[int64]revisions.Entry{1: activeEntry(1)}}
	objs := fleetWith(30, 10, false)
	c := gateController(t, []string{capTen}, resolver, objs...)
	held := 0
	for i := 10; i < 30; i++ {
		_, heldBy, _, err := c.selectRevision(context.Background(), pick(objs, i), resolver.latest)
		require.NoError(t, err)
		if heldBy != "" {
			held++
		}
	}
	assert.Equal(t, 20, held, "every pending instance is held while the 10 bad ones never recover")
}

// A held instance is frozen, not re-applied, so the revision it is on does
// not need to be resolvable: it stays held even when revision 1 is gone.
func TestFleetGate_HeldDoesNotNeedOldRevision(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(2)} // revision 1 not resolvable
	objs := fleetWith(30, 10, false)
	c := gateController(t, []string{capTen}, resolver, objs...)
	_, heldBy, admitted, err := c.selectRevision(context.Background(), pick(objs, 15), resolver.latest)
	require.NoError(t, err)
	assert.False(t, admitted)
	assert.NotEmpty(t, heldBy)
}

// Changing spec.propagateWhen must not issue a new revision.
func TestFleetGate_ExcludedFromSpecHash(t *testing.T) {
	a := v1alpha1.ResourceGraphDefinitionSpec{Schema: &v1alpha1.Schema{Kind: "WebApp", APIVersion: "v1alpha1"}}
	b := *a.DeepCopy()
	b.PropagateWhen = []string{capTen}
	ha, err := hashSpec(a)
	require.NoError(t, err)
	hb, err := hashSpec(b)
	require.NoError(t, err)
	assert.Equal(t, ha, hb)
}

var _ = types.UID("")

func hashSpec(s v1alpha1.ResourceGraphDefinitionSpec) (string, error) { return graphhash.Spec(s) }

// Ten workers evaluate the gate at the same instant against a fleet where
// nothing is in flight. Without serialization every worker sees 0 and admits;
// with the ledger exactly `cap` are admitted.
func TestFleetGate_ConcurrentWorkersRespectCap(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(2), older: map[int64]revisions.Entry{1: activeEntry(1)}}
	objs := fleetWith(30, 0, false) // all on rev 1, Ready
	c := gateController(t, []string{`${instances.filter(i, i.updated() && !i.ready()).size() < 5}`}, resolver, objs...)

	const workers = 10
	start := make(chan struct{})
	results := make(chan bool, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, ok, err := c.selectRevision(context.Background(), pick(objs, i), resolver.latest)
			require.NoError(t, err)
			results <- ok
		}(w)
	}
	close(start)
	wg.Wait()
	close(results)
	admitted := 0
	for ok := range results {
		if ok {
			admitted++
		}
	}
	assert.Equal(t, 5, admitted, "cap 5 with 10 concurrent workers must admit exactly 5")
	assert.Len(t, c.ledger.admitted, 5)
}

// A released admission frees its slot; a visible write drops the entry too.
func TestFleetGate_LedgerReleaseAndReap(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(2), older: map[int64]revisions.Entry{1: activeEntry(1)}}
	objs := fleetWith(30, 0, false)
	c := gateController(t, []string{`${instances.filter(i, i.updated() && !i.ready()).size() < 1}`}, resolver, objs...)

	_, _, admitted, err := c.selectRevision(context.Background(), pick(objs, 0), resolver.latest)
	require.NoError(t, err)
	require.True(t, admitted)
	_, heldBy, _, err := c.selectRevision(context.Background(), pick(objs, 1), resolver.latest)
	require.NoError(t, err)
	assert.NotEmpty(t, heldBy, "slot taken by app-00's admission")

	// app-00's reconcile fails → release → slot free.
	c.ledger.release(2, "default/app-00")
	_, _, admitted, err = c.selectRevision(context.Background(), pick(objs, 1), resolver.latest)
	require.NoError(t, err)
	assert.True(t, admitted)

	// app-01's write becomes visible on rev 2 (not Ready): ledger entry is
	// reaped as redundant, but the instance still counts via the snapshot.
	objs2 := fleetWith(30, 0, false)
	objs2[1] = fleetInstance(1, 2, false, "standard")
	c2 := gateController(t, []string{`${instances.filter(i, i.updated() && !i.ready()).size() < 1}`}, resolver, objs2...)
	c2.ledger.revision = 2
	c2.ledger.admitted = map[string]time.Time{"default/app-01": time.Now()}
	_, heldBy, _, err = c2.selectRevision(context.Background(), pick(objs2, 2), resolver.latest)
	require.NoError(t, err)
	assert.NotEmpty(t, heldBy)
	assert.NotContains(t, c2.ledger.admitted, "default/app-01", "visible write reaps the ledger entry")
}

// updated()/ready() on the fleet gate are defined on `instances` items and
// `each` only.
func TestFleetGate_ItemFactReceivers(t *testing.T) {
	for _, tc := range []struct {
		name, gate, wantErr string
	}{
		{"instances items", `${instances.filter(i, i.updated() && !i.ready()).size() < 10}`, ""},
		{"each", `${each.updated() || instances.all(i, i.ready())}`, ""},
		{"stray receiver", `${each.spec.replicas.updated()}`, ""}, // receiver root is `each`: allowed
		{"unrooted call", `${[1,2].filter(x, x.ready()).size() == 0}`, "ready() is only defined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := gateController(t, []string{tc.gate}, &fakeResolver{})
			_, err := c.compileFleetGate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestFleetGate_RequiresFeatureGate(t *testing.T) {
	require.NoError(t, features.FeatureGate.Set("PropagateWhen=false"))
	t.Cleanup(func() { _ = features.FeatureGate.Set("PropagateWhen=true") })
	c := gateController(t, []string{`${instances.size() < 10}`}, &fakeResolver{})
	_, err := c.compileFleetGate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PropagateWhen feature gate")
}

// #2: the fleet is every instance of the RGD across namespaces.
func TestFleetGate_FleetSpansNamespaces(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(2), older: map[int64]revisions.Entry{1: activeEntry(1)}}
	objs := fleetWith(30, 0, false)
	// 10 in flight, all in a different namespace from the instance under decision.
	for i := 0; i < 10; i++ {
		u := fleetInstance(i, 2, false, "standard")
		u.SetNamespace("other")
		u.SetUID(types.UID(fmt.Sprintf("other-%02d", i)))
		objs[i] = u
	}
	c := gateController(t, []string{capTen}, resolver, objs...)
	_, heldBy, admitted, err := c.selectRevision(context.Background(), pick(objs, 15), resolver.latest)
	require.NoError(t, err)
	assert.False(t, admitted)
	assert.NotEmpty(t, heldBy, "in-flight instances in another namespace fill the cap")
}

// Round-2 #3: one convergence wakes one held sibling, not all of them.
func TestFleetGate_TakeHeldIsBounded(t *testing.T) {
	l := &admissionLedger{}
	for i := 0; i < 5; i++ {
		l.markHeld(types.NamespacedName{Namespace: "default", Name: fmt.Sprintf("app-%02d", i)})
	}
	assert.Len(t, l.takeHeld(1), 1)
	assert.Len(t, l.held, 4)
	assert.Len(t, l.takeHeld(10), 4)
	assert.Empty(t, l.held)
}

// A held instance that already carries the exact Held condition is not
// rewritten on every evaluation.
func TestFleetGate_HeldConditionWriteIsIdempotent(t *testing.T) {
	inst := fleetInstance(0, 1, true, "standard")
	assert.False(t, heldConditionCurrent(inst, 1, 2, capTen))
	c := gateController(t, []string{capTen}, &fakeResolver{}, inst)
	require.NoError(t, c.writeHeldCondition(context.Background(), inst, 1, 2, capTen))
	got, err := c.instanceClient(inst).Get(context.Background(), inst.GetName(), metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, heldConditionCurrent(got, 1, 2, capTen))
	assert.False(t, heldConditionCurrent(got, 1, 3, capTen), "a new latest revision changes the message")
	assert.True(t, instanceConverged(got), "held does not make the instance !ready()")
	st, _, _ := unstructured.NestedString(got.Object, "status", "state")
	assert.NotEqual(t, "ERROR", st, "held does not set state=ERROR")
}

// Round-3 #1: publishing r3 while instances are mid-r2 must not freeze them.
// A frozen instance's Ready never changes, so any gate counting !ready() would
// deadlock. An instance not converged on its revision is never gated: it takes
// latest and counts as in flight there. The healthy r1 instances are then held
// by the ceiling (10 unready) until those converge.
func TestFleetGate_MidRolloutInstancesAreNeverHeld(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(3), older: map[int64]revisions.Entry{1: activeEntry(1), 2: activeEntry(2)}}
	objs := fleetWith(30, 0, false) // 30 on r1, ready
	for i := 0; i < 10; i++ {
		objs[i] = fleetInstance(i, 2, false, "standard") // mid-r2, not ready
	}
	for _, gate := range []string{
		`${instances.filter(i, !i.ready()).size() < 10}`,                 // ceiling
		`${instances.filter(i, !i.updated() && !i.ready()).size() == 0}`, // no existing issues
		`${instances.filter(i, i.updated() && !i.ready()).size() < 10}`,  // budget
	} {
		t.Run(gate, func(t *testing.T) {
			c := gateController(t, []string{gate}, resolver, objs...)
			admittedMid, admittedHealthy := 0, 0
			for i := 0; i < 30; i++ {
				_, _, ok, err := c.selectRevision(context.Background(), pick(objs, i), resolver.latest)
				require.NoError(t, err)
				if i < 10 && ok {
					admittedMid++
				}
				if i >= 10 && ok {
					admittedHealthy++
				}
			}
			assert.Equal(t, 10, admittedMid, "every mid-rollout instance takes r3 ungated")
			switch gate {
			case `${instances.filter(i, !i.updated() && !i.ready()).size() == 0}`:
				// The 10 are now in flight on r3, not broken on r2, so the guard
				// sees no existing issue and admits everything else. On the fleet a
				// broken instance always takes the latest revision; this guard only
				// ever sees broken instances transiently. The ceiling bounds disruption.
				assert.Equal(t, 20, admittedHealthy)
			default:
				assert.Equal(t, 0, admittedHealthy, "healthy instances wait while 10 are unready")
				assert.Len(t, c.ledger.admitted, 10, "mid-rollout admissions are in the ledger")
			}
		})
	}
	// Once the 10 converge on r3, healthy ones are admitted.
	for i := 0; i < 10; i++ {
		objs[i] = fleetInstance(i, 3, true, "standard")
	}
	c := gateController(t, []string{`${instances.filter(i, !i.ready()).size() < 10}`}, resolver, objs...)
	_, _, ok, err := c.selectRevision(context.Background(), pick(objs, 15), resolver.latest)
	require.NoError(t, err)
	assert.True(t, ok)
}

// A claimed instance (observedRevision advanced, convergedRevision not) is in
// flight. Its Ready condition is not consulted (it is stale-True right after a
// claim). claimRevision writes observedRevision and the informational
// RevisionReconciled=False/Updating in one patch and keeps other conditions.
func TestFleetGate_ClaimedInstanceCountsAsInFlight(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(2), older: map[int64]revisions.Entry{1: activeEntry(1)}}
	objs := fleetWith(30, 0, false)
	for i := 0; i < 10; i++ {
		objs[i] = fleetInstanceConv(i, 2, 1, true, "standard") // claimed r2, Ready still True from r1
	}
	c := gateController(t, []string{capTen}, resolver, objs...)
	_, heldBy, admitted, err := c.selectRevision(context.Background(), pick(objs, 15), resolver.latest)
	require.NoError(t, err)
	assert.False(t, admitted)
	assert.NotEmpty(t, heldBy, "10 claimed-but-unconverged instances fill the cap")

	inst := fleetInstance(40, 1, true, "standard")
	c = gateController(t, []string{capTen}, &fakeResolver{}, inst)
	require.NoError(t, c.claimRevision(context.Background(), inst, 2))
	got, err := c.instanceClient(inst).Get(context.Background(), inst.GetName(), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(2), observedRevision(got))
	assert.Equal(t, int64(1), convergedRevision(got), "claim does not touch convergedRevision")
	assert.True(t, inFlight(got))
	assert.False(t, instanceFacts(got, 2)[library.ItemReadyKey].(bool), "gate fact is !ready while in flight, whatever Ready says")
	conds, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
	types := map[string]string{}
	for _, cnd := range conds {
		m := cnd.(map[string]any)
		types[m["type"].(string)] = m["status"].(string)
	}
	assert.Equal(t, "True", types[string(v1alpha1.InstanceConditionTypeReady)], "Ready is preserved")
	assert.Equal(t, "False", types[RevisionReconciled])
}

// A held instance is converged on its revision and frozen. Its Ready is the
// truth. Ready=True held instances are neither in flight nor Stuck.
func TestFleetGate_HeldInstancesAreNotStuck(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(2), older: map[int64]revisions.Entry{1: activeEntry(1)}}
	objs := fleetWith(30, 10, true) // 10 converged on r2
	for i := 10; i < 30; i++ {
		u := fleetInstance(i, 1, true, "standard")
		conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
		conds = append(conds, map[string]any{"type": RevisionReconciled, "status": "False", "reason": "Held", "message": heldMessage(1, 2, capTen)})
		_ = unstructured.SetNestedSlice(u.Object, conds, "status", "conditions")
		objs[i] = u
	}
	held := pick(objs, 15)
	assert.False(t, inFlight(held))
	for _, gate := range []string{
		`${instances.filter(i, !i.ready()).size() < 10}`,
		`${instances.filter(i, !i.updated() && !i.ready()).size() == 0}`,
	} {
		c := gateController(t, []string{gate}, resolver, objs...)
		_, heldBy, admitted, err := c.selectRevision(context.Background(), held, resolver.latest)
		require.NoError(t, err)
		assert.True(t, admitted, gate)
		assert.Empty(t, heldBy)
	}
}

// Round-4: the in-flight bypass is keyed on observed != converged, not on
// Ready=False. A converged instance that blipped unready (drain, HPA) is gated
// and counted like any other, so a correlated event cannot blow through the
// cap. Reviewer's three overlays.
func TestFleetGate_BlippedInstancesAreGated(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(2), older: map[int64]revisions.Entry{1: activeEntry(1)}}

	t.Run("budget with 10 in flight and 15 blipped on r1 admits none", func(t *testing.T) {
		objs := fleetWith(30, 10, false) // 10 in flight on r2
		for i := 10; i < 25; i++ {
			objs[i] = fleetInstanceBlipped(i, 1, "standard")
		}
		c := gateController(t, []string{capTen}, resolver, objs...)
		admitted := 0
		for i := 10; i < 30; i++ {
			_, _, ok, err := c.selectRevision(context.Background(), pick(objs, i), resolver.latest)
			require.NoError(t, err)
			if ok {
				admitted++
			}
		}
		assert.Equal(t, 0, admitted, "cap is full; blipped instances are not a bypass")
	})

	t.Run("${false} gate never admits a blipped instance", func(t *testing.T) {
		objs := fleetWith(30, 0, false)
		objs[5] = fleetInstanceBlipped(5, 1, "standard")
		c := gateController(t, []string{`${false}`}, resolver, objs...)
		_, heldBy, ok, err := c.selectRevision(context.Background(), pick(objs, 5), resolver.latest)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.NotEmpty(t, heldBy)
	})

	t.Run("canary-first holds a blipped standard instance", func(t *testing.T) {
		canaryFirst := `${each.spec.tier == 'canary' || instances.filter(i, i.spec.tier == 'canary' && !(i.updated() && i.ready())).size() == 0}`
		objs := fleetWith(30, 0, false)
		objs[0] = fleetInstance(0, 1, true, "canary")
		objs[7] = fleetInstanceBlipped(7, 1, "standard")
		c := gateController(t, []string{canaryFirst}, resolver, objs...)
		_, _, ok, err := c.selectRevision(context.Background(), pick(objs, 7), resolver.latest)
		require.NoError(t, err)
		assert.False(t, ok, "standard waits for the canary even when unready")
		_, _, ok, err = c.selectRevision(context.Background(), pick(objs, 0), resolver.latest)
		require.NoError(t, err)
		assert.True(t, ok, "canary goes first")
	})

	t.Run("genuinely in-flight instance still bypasses", func(t *testing.T) {
		objs := fleetWith(30, 0, false)
		objs[3] = fleetInstance(3, 2, false, "standard") // admitted r2, converged r1
		resolver3 := fakeResolver{latest: activeEntry(3), older: map[int64]revisions.Entry{1: activeEntry(1), 2: activeEntry(2)}}
		c := gateController(t, []string{`${false}`}, resolver3, objs...)
		_, _, ok, err := c.selectRevision(context.Background(), pick(objs, 3), resolver3.latest)
		require.NoError(t, err)
		assert.True(t, ok, "mid-rollout instance takes latest ungated")
	})
}

// Round-5: a held instance is frozen, so kro does not know its current health.
// Fleet ready() therefore means "converged on the revision it was admitted to"
// and does not read the Ready condition. An instance that blipped unready at
// publish time is converged on r1: it is a candidate, it is held when the cap
// is full, and it does not pin any gate that counts !ready(). Reviewer's
// overlays: guard with 1 blipped, ceiling with 10 blipped, both admit.
func TestFleetGate_BlippedHeldInstancesDoNotDeadlock(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(2), older: map[int64]revisions.Entry{1: activeEntry(1)}}

	t.Run("guard with 1 blipped instance admits", func(t *testing.T) {
		objs := fleetWith(30, 0, false)
		objs[5] = fleetInstanceBlipped(5, 1, "standard")
		c := gateController(t, []string{`${instances.filter(i, !i.updated() && !i.ready()).size() == 0}`}, resolver, objs...)
		_, heldBy, ok, err := c.selectRevision(context.Background(), pick(objs, 15), resolver.latest)
		require.NoError(t, err)
		assert.True(t, ok, "a blipped instance is converged on r1 and not counted as broken")
		assert.Empty(t, heldBy)
	})

	t.Run("ceiling with 10 blipped instances admits", func(t *testing.T) {
		objs := fleetWith(30, 0, false)
		for i := 0; i < 10; i++ {
			objs[i] = fleetInstanceBlipped(i, 1, "standard")
		}
		c := gateController(t, []string{`${instances.filter(i, !i.ready()).size() < 10}`}, resolver, objs...)
		admitted := 0
		for i := 0; i < 30; i++ {
			_, _, ok, err := c.selectRevision(context.Background(), pick(objs, i), resolver.latest)
			require.NoError(t, err)
			if ok {
				admitted++
			}
		}
		assert.Equal(t, 10, admitted, "ceiling counts in-flight instances only: first 10 admitted, then full")
	})

	t.Run("blipped instance itself is gated and counted like any other", func(t *testing.T) {
		// Round-4 property preserved: blipped is not a bypass.
		objs := fleetWith(30, 10, false) // 10 in flight
		objs[15] = fleetInstanceBlipped(15, 1, "standard")
		c := gateController(t, []string{capTen}, resolver, objs...)
		_, _, ok, err := c.selectRevision(context.Background(), pick(objs, 15), resolver.latest)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("never-converged instance is in flight, not converged", func(t *testing.T) {
		u := fleetInstance(0, 1, false, "standard")
		unstructured.RemoveNestedField(u.Object, "status", "convergedRevision")
		assert.False(t, instanceConverged(u))
		assert.False(t, instanceFacts(u, 2)[library.ItemReadyKey].(bool))
		assert.True(t, instanceFacts(u, 2)[library.ItemUpdatedKey].(bool), "in flight counts as updated() for the budget")
	})
}
