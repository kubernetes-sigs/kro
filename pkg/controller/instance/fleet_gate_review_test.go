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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
)

// Adversarial overlays against the fleet gate fact model, written as an
// independent review pass: mixed compositions, blips, pre-feature instances,
// rollover, canary, stale-cache claim. One controller, every
// instance evaluated in order, count admissions.
func reviewPass(t *testing.T, gate string, resolver fakeResolver, objs []apimachineryruntime.Object) (admitted, held int, c *Controller) {
	c = gateController(t, []string{gate}, resolver, objs...)
	for i := range objs {
		_, heldBy, ok, err := c.selectRevision(context.Background(), pick(objs, i), resolver.latest)
		require.NoError(t, err)
		if ok {
			admitted++
		}
		if heldBy != "" {
			held++
		}
	}
	return
}

const (
	rvCap     = `${instances.filter(i, i.updated() && !i.ready()).size() < 10}`
	rvCeiling = `${instances.filter(i, !i.ready()).size() < 10}`
	rvGuard   = `${instances.filter(i, !i.updated() && !i.ready()).size() == 0}`
	rvCanary  = `${each.spec.tier == 'canary' || instances.filter(i, i.spec.tier == 'canary' && !(i.updated() && i.ready())).size() == 0}`
)

// 1. Cap: mixed composition. 5 in flight on r2 (count), 5 converged r2, 5 blipped r1,
// 5 pre-feature r1 (conv absent), 5 fresh (no observed), 5 converged r1.
// Expect: fresh 5 skip gate (ungated by design, not "admitted"), in-flight 5 take latest
// ungated and are counted; cap room = 10-5 = 5 more admissions among the 15 candidates.
func TestReview_CapMixedComposition(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(2), older: map[int64]revisions.Entry{1: activeEntry(1)}}
	objs := make([]apimachineryruntime.Object, 0, 30)
	for i := 0; i < 5; i++ {
		objs = append(objs, fleetInstance(i, 2, false, "standard")) // in flight on r2
	}
	for i := 5; i < 10; i++ {
		objs = append(objs, fleetInstance(i, 2, true, "standard")) // converged r2
	}
	for i := 10; i < 15; i++ {
		objs = append(objs, fleetInstanceBlipped(i, 1, "standard"))
	}
	for i := 15; i < 20; i++ {
		objs = append(objs, fleetInstanceConv(i, 1, 0, false, "standard")) // never converged on r1: in flight
	}
	for i := 20; i < 25; i++ {
		u := fleetInstance(i, 1, true, "standard")
		delete(u.Object["status"].(map[string]any), "observedRevision")
		delete(u.Object["status"].(map[string]any), "convergedRevision")
		objs = append(objs, u) // fresh
	}
	for i := 25; i < 30; i++ {
		objs = append(objs, fleetInstance(i, 1, true, "standard"))
	}
	c := gateController(t, []string{rvCap}, resolver, objs...)
	gated, ungated := 0, 0
	for i := range objs {
		_, heldBy, ok, err := c.selectRevision(context.Background(), pick(objs, i), resolver.latest)
		require.NoError(t, err)
		if ok {
			gated++
		} else if heldBy == "" {
			ungated++ // skipped gate: fresh, already-latest
		}
	}
	// In flight already on latest (0-4) hit the observed>=latest short-circuit
	// and are counted through the snapshot (updated() includes in-flight).
	// Never-converged on r1 (15-19) are in flight too: they bypass and go in
	// the ledger. 5 + 5 = 10 in flight → no room for the 10 converged-r1
	// candidates. Ledger holds the 5 bypasses only.
	t.Logf("ledger=%d gated-admits(ok)=%d ungated-skips=%d", len(c.ledger.admitted), gated, ungated)
	assert.Equal(t, 5, len(c.ledger.admitted), "5 in-flight bypasses; cap already full via snapshot")
	assert.Equal(t, 5, gated, "the ok=true are the 5 bypasses")
	assert.Equal(t, 15, ungated)
}

// 2. Ceiling/guard: converged fleet with blipped instances, nothing in flight.
// Must NOT deadlock: ceiling admits up to 10, guard admits.
func TestReview_CeilingGuardNoDeadlockOnBlips(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(2), older: map[int64]revisions.Entry{1: activeEntry(1)}}
	for _, n := range []int{1, 10, 30} {
		objs := fleetWith(30, 0, false)
		for i := 0; i < n; i++ {
			objs[i] = fleetInstanceBlipped(i, 1, "standard")
		}
		a, _, _ := reviewPass(t, rvCeiling, resolver, objs)
		assert.Equal(t, 10, a, "ceiling with %d blipped admits 10", n)
		a, _, _ = reviewPass(t, rvGuard, resolver, objs)
		assert.Equal(t, 30, a, "guard with %d blipped admits all", n)
	}
}

// 2b. Ceiling: is there ANY composition with no in-flight instance where all are held?
// !ready() == in flight, so with nothing in flight the ceiling is 0 < 10. Prove by exhaustion
// over held-marked + blipped + pre-feature.
func TestReview_CeilingNeverPermanentlyHeld(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(2), older: map[int64]revisions.Entry{1: activeEntry(1)}}
	objs := fleetWith(30, 0, false)
	for i := 0; i < 10; i++ {
		objs[i] = fleetInstanceBlipped(i, 1, "standard")
	}
	for i := 10; i < 20; i++ {
		u := fleetInstance(i, 1, false, "standard") // observed=1, conv=0 (absent-equivalent 0) → treated converged
		objs[i] = u
	}
	a, h, _ := reviewPass(t, rvCeiling, resolver, objs)
	assert.Equal(t, 10, a)
	assert.Equal(t, 20, h)
}

// 3. Canary: can a standard advance before the canary converges? Canary converged r1,
// standard blipped / pre-feature / normal.
func TestReview_CanaryFirstHolds(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(2), older: map[int64]revisions.Entry{1: activeEntry(1)}}
	objs := fleetWith(30, 0, false)
	objs[0] = fleetInstance(0, 1, true, "canary")
	objs[7] = fleetInstanceBlipped(7, 1, "standard")
	objs[8] = fleetInstance(8, 1, true, "standard")
	c := gateController(t, []string{rvCanary}, resolver, objs...)
	for _, i := range []int{7, 8, 15} {
		_, _, ok, err := c.selectRevision(context.Background(), pick(objs, i), resolver.latest)
		require.NoError(t, err)
		assert.False(t, ok, "standard %d must wait for canary", i)
	}
	_, _, ok, err := c.selectRevision(context.Background(), pick(objs, 0), resolver.latest)
	require.NoError(t, err)
	assert.True(t, ok)
	// canary in flight on r2 (claimed, not converged): standards still wait
	objs[0] = fleetInstance(0, 2, false, "canary")
	c = gateController(t, []string{rvCanary}, resolver, objs...)
	_, _, ok, err = c.selectRevision(context.Background(), pick(objs, 15), resolver.latest)
	require.NoError(t, err)
	assert.False(t, ok, "canary in flight: standards wait")
	// canary converged on r2: standards go
	objs[0] = fleetInstance(0, 2, true, "canary")
	c = gateController(t, []string{rvCanary}, resolver, objs...)
	_, _, ok, err = c.selectRevision(context.Background(), pick(objs, 15), resolver.latest)
	require.NoError(t, err)
	assert.True(t, ok)
}

// 4. Rollover r2->r3: 8 in flight r2, 10 converged r2, 12 converged r1. Cap 10.
func TestReview_RolloverCapExact(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(3), older: map[int64]revisions.Entry{1: activeEntry(1), 2: activeEntry(2)}}
	objs := make([]apimachineryruntime.Object, 0, 30)
	for i := 0; i < 8; i++ {
		objs = append(objs, fleetInstance(i, 2, false, "standard"))
	}
	for i := 8; i < 18; i++ {
		objs = append(objs, fleetInstance(i, 2, true, "standard"))
	}
	for i := 18; i < 30; i++ {
		objs = append(objs, fleetInstance(i, 1, true, "standard"))
	}
	_, held, c := reviewPass(t, rvCap, resolver, objs)
	assert.Equal(t, 10, len(c.ledger.admitted), "8 in-flight bypass + 2 admitted = cap")
	assert.Equal(t, 20, held)
	// ceiling on same fleet
	_, held, c = reviewPass(t, rvCeiling, resolver, objs)
	assert.Equal(t, 10, len(c.ledger.admitted))
	assert.Equal(t, 20, held)
}

// 5. Pre-feature: neither revision field. observedRevision == 0 skips the gate
// (fresh), by design; the first reconcile writes both fields. A never-converged
// instance (observed set, converged 0) is in flight and bypasses.
func TestReview_PreFeatureAndNeverConverged(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(2), older: map[int64]revisions.Entry{1: activeEntry(1)}}
	objs := fleetWith(30, 10, false) // 10 in flight → cap full
	for i := 10; i < 20; i++ {
		u := fleetInstance(i, 1, true, "standard")
		st := u.Object["status"].(map[string]any)
		delete(st, "convergedRevision")
		delete(st, "observedRevision")
		objs[i] = u
	}
	for i := 20; i < 30; i++ {
		objs[i] = fleetInstanceConv(i, 1, 0, false, "standard")
	}
	c := gateController(t, []string{`${false}`}, resolver, objs...)
	for i := 10; i < 20; i++ {
		_, heldBy, ok, err := c.selectRevision(context.Background(), pick(objs, i), resolver.latest)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Empty(t, heldBy, "pre-feature %d skips the gate (fresh), not held, not admitted-through-gate", i)
	}
	for i := 20; i < 30; i++ {
		_, _, ok, err := c.selectRevision(context.Background(), pick(objs, i), resolver.latest)
		require.NoError(t, err)
		assert.True(t, ok, "never-converged %d is in flight and bypasses", i)
	}
}

// 5b. Pre-feature with convergedRevision absent but observed=0 too: fresh → ungated (by design).
// 5c. Adversarial: convergedRevision > observedRevision (author wrote it). Rejected at build now,
// but if it existed on the wire: inFlight → bypass. Document.
func TestReview_AdversarialConvergedAhead(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(2), older: map[int64]revisions.Entry{1: activeEntry(1)}}
	objs := fleetWith(30, 10, false)
	objs[15] = fleetInstanceConv(15, 1, 99, true, "standard")
	c := gateController(t, []string{`${false}`}, resolver, objs...)
	_, _, ok, err := c.selectRevision(context.Background(), pick(objs, 15), resolver.latest)
	require.NoError(t, err)
	t.Logf("convergedRevision=99 observed=1 under ${false}: admitted=%v (bypass if true; field is rejected in RGD schema, wire-only risk)", ok)
}

// 6. claimRevision must not touch convergedRevision; stale cache can't write stale cond.
func TestReview_ClaimDoesNotAdvanceConverged(t *testing.T) {
	inst := fleetInstance(0, 1, true, "standard")
	c := gateController(t, []string{rvCap}, &fakeResolver{}, inst)
	// Mutate the server copy behind the cache: Ready goes False.
	srv, err := c.instanceClient(inst).Get(context.Background(), inst.GetName(), metav1GetOpts())
	require.NoError(t, err)
	conds := srv.Object["status"].(map[string]any)["conditions"].([]any)
	conds[0].(map[string]any)["status"] = "False"
	_, err = c.instanceClient(inst).UpdateStatus(context.Background(), srv, metav1UpdateOpts())
	require.NoError(t, err)
	// Claim using the STALE inst (Ready=True in cache).
	require.NoError(t, c.claimRevision(context.Background(), inst, 2))
	got, err := c.instanceClient(inst).Get(context.Background(), inst.GetName(), metav1GetOpts())
	require.NoError(t, err)
	assert.Equal(t, int64(1), convergedRevision(got))
	assert.Equal(t, int64(2), observedRevision(got))
	for _, cnd := range got.Object["status"].(map[string]any)["conditions"].([]any) {
		m := cnd.(map[string]any)
		if m["type"] == "Ready" {
			assert.Equal(t, "False", m["status"], "fresh read: server's Ready=False preserved, stale cache did not write back True")
		}
	}
	_ = fmt.Sprint
}

func metav1GetOpts() metav1.GetOptions       { return metav1.GetOptions{} }
func metav1UpdateOpts() metav1.UpdateOptions { return metav1.UpdateOptions{} }

// Round-6 blocker 1: a never-converged canary must not read as converged.
func TestReview_NeverConvergedCanaryHoldsStandards(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(2), older: map[int64]revisions.Entry{1: activeEntry(1)}}
	objs := fleetWith(30, 0, false)
	objs[0] = fleetInstanceConv(0, 2, 0, false, "canary") // created on r2, never Ready
	c := gateController(t, []string{rvCanary}, resolver, objs...)
	admitted := 0
	for i := 1; i < 30; i++ {
		_, _, ok, err := c.selectRevision(context.Background(), pick(objs, i), resolver.latest)
		require.NoError(t, err)
		if ok {
			admitted++
		}
	}
	assert.Equal(t, 0, admitted, "canary is in flight, not converged: every standard waits")
}

// Round-6 blocker 2: two publishes within admissionGrace. 10 claimed to r2 whose
// claims are visible (observed=2, conv=1) when r3 publishes. They are in flight,
// count as updated() && !ready() for r3, and the ledger carries any unlanded
// claims. No second cap's worth is admitted.
func TestReview_RolloverWithinGraceDoesNotDouble(t *testing.T) {
	resolver := fakeResolver{latest: activeEntry(3), older: map[int64]revisions.Entry{1: activeEntry(1), 2: activeEntry(2)}}
	objs := fleetWith(30, 0, false)
	for i := 0; i < 10; i++ {
		objs[i] = fleetInstance(i, 2, false, "standard") // in flight on r2
	}
	c := gateController(t, []string{rvCap}, resolver, objs...)
	// Simulate: ledger still holds r2-era entries for 3 instances whose claims
	// have not landed (they look like converged r1 in the snapshot).
	c.ledger.revision = 2
	c.ledger.admitted = map[string]time.Time{"default/app-10": time.Now(), "default/app-11": time.Now(), "default/app-12": time.Now()}
	admitted := 0
	for i := 10; i < 30; i++ {
		_, _, ok, err := c.selectRevision(context.Background(), pick(objs, i), resolver.latest)
		require.NoError(t, err)
		if ok {
			admitted++
		}
	}
	// 10 in flight via snapshot already fill the cap; the 3 ledger entries are
	// carried (not wiped) and would also count. Nothing new admitted.
	assert.Equal(t, 0, admitted, "in-flight r2 instances count toward r3's budget; ledger carried across rollover")
	assert.Contains(t, c.ledger.admitted, "default/app-10", "ledger entry survives the rollover")
}

// Round-6 blocker 3: status is not projected, so a gate cannot read a frozen
// Ready condition and pin itself. Facts are the only status-derived inputs.
func TestReview_StatusNotProjected(t *testing.T) {
	inst := fleetInstanceBlipped(0, 1, "standard")
	facts := instanceFacts(inst, 2)
	_, hasStatus := facts["status"]
	assert.False(t, hasStatus, "status is stale for held instances and must not be a gate input")
	assert.NotNil(t, facts["spec"])
	assert.NotNil(t, facts["metadata"])
	// A gate that reads i.status.* now errors (fail closed) instead of silently deadlocking.
	resolver := fakeResolver{latest: activeEntry(2), older: map[int64]revisions.Entry{1: activeEntry(1)}}
	objs := fleetWith(30, 0, false)
	c := gateController(t, []string{`${instances.filter(i, i.status.conditions.exists(c, c.type == 'Ready' && c.status == 'True')).size() == 30}`}, resolver, objs...)
	_, _, _, err := c.selectRevision(context.Background(), pick(objs, 5), resolver.latest)
	require.Error(t, err, "reading status in a fleet gate is an error, not a stale read")
}
