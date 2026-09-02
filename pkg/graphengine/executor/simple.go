// Copyright 2026 The Kubernetes Authors.
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

package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/applyset"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// defaultApplyConcurrency is the default concurrency limit for parallel
// collection apply operations when Simple.ApplyConcurrency is <= 0.
const defaultApplyConcurrency = 20

// Simple walks nodes in topological order, SSA-applies
// each Template, records observed state on the runtime so dependents see
// the live cluster values, and on Delete tears them down in reverse.
//
// Ignored nodes (includeWhen=false, or contagiously via an ignored
// upstream) are skipped entirely — no resolve, no apply, no scope
// publication. ReadyWhen checks gate the loop: an unsatisfied readyWhen
// returns ErrNotReady so the reconciler requeues.
type Simple struct {
	Client client.Client
	// LabelInjector, when non-nil, is called on every child object
	// immediately before SSA apply so kro's per-instance labels are
	// stamped by the graph-engine path. Safe to leave nil — no-op.
	LabelInjector func(*unstructured.Unstructured)
	// GateReadiness makes the executor withhold a node until every one of
	// its dependencies has reached a terminal ready state this cycle. When
	// false (the default), every reachable node is applied regardless of
	// upstream readiness so drift watches register across a not-ready node.
	GateReadiness bool
	// ApplyConcurrency bounds the number of concurrent SSA apply operations
	// executed in parallel for collection nodes. 0 means use defaultApplyConcurrency.
	ApplyConcurrency int
	// ConflictDetection, when true, makes Template applies use a per-Graph SSA
	// field manager and refuse to force-steal a field already owned by another
	// kro Graph's template manager (surfaced as ErrFieldManagerConflict, a soft
	// not-ready). External drift (kubectl, other controllers) is still reclaimed
	// by a forced re-apply. Off by default: the RGD/instance path relies on the
	// shared FieldManager plus its ApplySet part-of ownership guard. Enabled only
	// for the standalone Graph controller, whose objects carry no ApplySet
	// part-of label, so without this two Graphs templating the same object under
	// the shared forced FieldManager would flip-flop its fields forever.
	ConflictDetection bool
	// CanWatch, when non-nil, gates drift-watch registration on whether the
	// identity this executor applies under may watch the target GVR. Returns
	// (false, nil) when not permitted (the watch is skipped, drift detection
	// degraded), (true, nil) when permitted, or a non-nil error on an
	// inconclusive check (treated as skip). Nil (RGD/instance path, controller
	// identity) means no gate — every watch is attempted.
	CanWatch func(ctx context.Context, gvr schema.GroupVersionResource, namespace string) (bool, error)
	// nodePrefix qualifies node IDs with their enclosing subgraph path so
	// identities stay unambiguous across frames that reuse the same local
	// node ID (e.g. "res" declared inside both subgraph "subA" and "subB").
	// Empty at the root. A child executor built by applySubgraph extends it
	// with the owning subgraph node's ID and a '/' separator, so it reads
	// like "subA/" (one level) or "subA/subB/" (nested). Every identity sink
	// — the coordinator watch key, the kro.run/node-id label/selector, and
	// the node-path annotation — derives from this via qualifiedPath /
	// nodeIDToken so all three agree by construction.
	nodePrefix string
	// identityClaims records which node has claimed each rendered object
	// identity (GVK+namespace+name) during a single Apply walk, so a SECOND
	// node rendering the same identity is refused BEFORE its SSA write instead
	// of clobbering the first node's object (and, for the standalone-Graph
	// path, before two same-Graph template managers force-reclaim each other's
	// fields forever while the Graph reports Ready). It is created per top-level
	// Apply and shared across subgraph child walks (applySubgraph copies the
	// executor by value, carrying this pointer) so a collision between template
	// nodes in different frames targeting one cluster object is caught too.
	// Nil outside an Apply walk; the post-apply validateAppliedIdentities check
	// remains as a backstop.
	identityClaims *identityClaimSet
	// OnToleratedRejection, when non-nil, is invoked for each collection item
	// whose UPDATE was rejected on an already-existing object and tolerated
	// (the live object is kept and the node still converges). It is a purely
	// OBSERVATIONAL hook — it must not influence readiness or requeue, or the
	// anti-wedge tolerance would be lost. The instance controller wires it to an
	// event recorder (Warning event); the Graph controller leaves it nil
	// (log-only, it has no recorder). Called from parallel apply goroutines, so
	// the implementation must be safe for concurrent use (an EventRecorder is).
	OnToleratedRejection func(ToleratedRejection)
}

// identityClaimSet is the per-Apply set of claimed object identities, guarded by
// a mutex because collection items apply in parallel.
type identityClaimSet struct {
	mu    sync.Mutex
	owner map[string]string // identity key -> qualified node ID that claimed it
}

// claim records that qualifiedNodeID intends to write the object identified by
// identityKey. It returns a non-nil error when that identity was ALREADY claimed
// this walk — the caller turns that into a hard error before any write. Every
// claim is fresh: the claim set is created once per top-level Apply (see Apply),
// and a single walk visits each node exactly once, so a legitimate write never
// re-claims an identity. A second claim therefore always means two rows collide
// on one final object — whether from two different nodes, or from a SINGLE
// collection node whose forEach produced two rows that resolve to the same
// identity (e.g. namespaces "" and "default" defaulting to the same object).
// Both are rejected: concurrent writes to one object let a nondeterministic
// last-writer win while the node still reports success.
func (c *identityClaimSet) claim(identityKey, qualifiedNodeID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if owner, ok := c.owner[identityKey]; ok {
		if owner == qualifiedNodeID {
			return fmt.Errorf("%w: node %q renders %s more than once",
				ErrDuplicateIdentity, qualifiedNodeID, identityKey)
		}
		return fmt.Errorf("%w: nodes %q and %q both render %s",
			ErrDuplicateIdentity, owner, qualifiedNodeID, identityKey)
	}
	c.owner[identityKey] = qualifiedNodeID
	return nil
}

// NewSimple constructs a Simple executor bound to the given client.
func NewSimple(c client.Client) *Simple {
	return &Simple{Client: c}
}

// WithLabelInjector sets a labeling function that is called on each child
// object before SSA apply. Calling this a second time replaces the previous
// injector. Returns the receiver for chaining.
func (s *Simple) WithLabelInjector(fn func(*unstructured.Unstructured)) *Simple {
	s.LabelInjector = fn
	return s
}

// WithConflictDetection toggles per-Graph field-manager conflict detection for
// Template applies (see the ConflictDetection field). Returns the receiver for
// chaining.
func (s *Simple) WithConflictDetection(on bool) *Simple {
	s.ConflictDetection = on
	return s
}

// ApplyWithLabeler is like Apply but stamps every child object with
// the supplied labeler just before SSA, in addition to the struct-level
// LabelInjector (if any). Uses a per-call override via the context so
// concurrent reconciles for different instances of the same GVR are safe.
func (s *Simple) ApplyWithLabeler(
	ctx context.Context,
	rt *runtime.Runtime,
	w watchrouter.Watcher,
	extraLabeler func(*unstructured.Unstructured),
) (ApplyResult, error) {
	if extraLabeler == nil {
		return s.Apply(ctx, rt, w)
	}
	// Compose: first apply struct-level labels, then per-call extra labels.
	prev := s.LabelInjector
	composed := func(obj *unstructured.Unstructured) {
		if prev != nil {
			prev(obj)
		}
		extraLabeler(obj)
	}
	// Build a shadow executor with the composed injector so we don't mutate s.
	shadow := *s
	shadow.LabelInjector = composed
	return shadow.Apply(ctx, rt, w)
}

var _ Interface = (*Simple)(nil)

// Apply walks rt in topological order. For each node it checks ignore
// status (contagious), resolves the desired state, registers a watch on
// the resulting GVR/Name/Namespace via w, applies to cluster (or just
// renders for Def), records observed state on the node, and finally
// checks readyWhen — surfacing an unsatisfied result as ErrNotReady so
// the reconciler requeues without backoff.
//
// Per-template watches are registered BEFORE SSA apply. Doing it before
// closes the window where an external actor could mutate the object
// between apply and watch registration — the informer cache picks up
// the next change either way, but the watch must exist first or the
// event gets dropped.
//
// Soft errors (ErrDataPending from Resolve, ErrWaitingForReadiness from
// CheckReadiness) do NOT abort the walk. The reconciler relies on every
// reachable node getting its watch declared so drift detection stays
// authoritative — bailing early on a not-ready upstream node would
// leave downstream nodes' watches missing, and the next reconcile
// would lose drift events on them. Soft errors are remembered and the
// first one is returned at the end wrapped in ErrNotReady. Hard errors
// (apply failure, type errors, etc.) still abort immediately.
//
// Dependency-readiness gating: a node is applied only once every node it
// depends on is ready this cycle (readyWhen satisfied). A dependency that
// was applied-but-not-ready, blocked, or unresolved leaves the dependent
// blocked too — it is recorded Unresolved and skipped (never applied) so a
// dependent resource is not created before its dependencies converge, and
// its own dependents cascade-block via the readiness map. Gating is opt-in
// via GateReadiness; when it is off every reachable node is applied
// regardless of upstream readiness.
func (s *Simple) Apply(ctx context.Context, rt *runtime.Runtime, w watchrouter.Watcher) (ApplyResult, error) {
	// Establish the per-Apply identity-claim set at the top-level walk. A
	// subgraph child walk (applySubgraph copies the executor by value) inherits
	// this non-nil pointer and shares the same set, so a template-node identity
	// collision is detected across frames. Guard on nil so only the outermost
	// Apply creates it.
	if s.identityClaims == nil {
		child := *s
		child.identityClaims = &identityClaimSet{owner: map[string]string{}}
		return child.Apply(ctx, rt, w)
	}

	var result ApplyResult
	var firstSoft error
	recordSoft := func(err error) {
		if firstSoft == nil {
			firstSoft = err
		}
	}
	// hardErrs collects per-node HARD failures. A hard error on one node must
	// NOT abort the whole walk: independent nodes and the trailing synthesized
	// author-status patch node still need to run. The failing node is never
	// marked ready, so its dependents stay gated (GateReadiness); the aggregate
	// hard error is returned after the walk and dominates any soft signal.
	var hardErrs []error
	recordHard := func(err error) { hardErrs = append(hardErrs, err) }

	// ready tracks which nodes reached a terminal ready state this cycle, so
	// dependents can be gated until their dependencies converge.
	ready := make(map[string]bool, len(rt.Nodes()))

	for _, n := range rt.Nodes() {
		ignored, err := n.IsIgnored()
		if err != nil {
			if isSoftRuntimeErr(err) {
				// includeWhen referenced data the cluster hasn't
				// produced yet. We can't decide whether to apply,
				// so identities are unknown — caller preserves the
				// previous entries for this NodeID.
				result.Unresolved = append(result.Unresolved, n.ID())
				recordSoft(fmt.Errorf("apply %q: includeWhen: %w (%w)", n.ID(), err, ErrNotReady))
				continue
			}
			recordHard(fmt.Errorf("apply %q: %w", n.ID(), err))
			continue
		}
		if ignored {
			// Intentionally skipped — not Unresolved. The caller
			// will treat any previous entries for this NodeID as
			// prune candidates. Ignored nodes are non-blocking for
			// dependents (their dependents are contagiously ignored too).
			ready[n.ID()] = true
			continue
		}

		// Gate on dependency readiness: do not apply until every dependency is
		// ready this cycle. Opt-in — when off, dependents apply across a
		// not-ready upstream (drift watches still register).
		if s.GateReadiness {
			if dep, blocked := firstUnreadyDep(n, ready); blocked {
				result.Unresolved = append(result.Unresolved, n.ID())
				recordSoft(fmt.Errorf("apply %q: waiting for dependency %q: %w", n.ID(), dep, ErrNotReady))
				continue
			}
		}

		// A subgraph node has no payload of its own — it runs a child
		// Program in a scope seeded from this one. Handle it before
		// Resolve (which would deref the nil Object).
		if n.Kind() == compiler.NodeKindGraph {
			applied, unresolved, contribs, err := s.applySubgraph(ctx, rt, w, n)
			result.Applied = append(result.Applied, applied...)
			result.Unresolved = append(result.Unresolved, unresolved...)
			result.Contributions = append(result.Contributions, contribs...)
			if err != nil {
				if errors.Is(err, ErrNotReady) || isSoftRuntimeErr(err) {
					recordSoft(fmt.Errorf("apply %q (subgraph): %w", n.ID(), err))
					continue
				}
				recordHard(fmt.Errorf("apply %q (subgraph): %w", n.ID(), err))
				continue
			}
			ready[n.ID()] = true
			continue
		}

		desired, err := n.Resolve()
		if err != nil {
			if isSoftRuntimeErr(err) {
				result.Unresolved = append(result.Unresolved, n.ID())
				recordSoft(fmt.Errorf("apply %q: resolve: %w (%w)", n.ID(), err, ErrNotReady))
				continue
			}
			recordHard(fmt.Errorf("apply %q: resolve: %w", n.ID(), err))
			continue
		}

		softErr, err := s.applyNodeByKind(ctx, rt, w, n, desired, &result)
		if err != nil {
			recordHard(err)
			continue
		}
		if softErr != nil {
			recordSoft(softErr)
			continue
		}

		// readyWhen is checked after observed state is recorded.
		// Soft → ErrNotReady (already tracked in Applied); continue
		// so downstream watches still register. Hard → aggregate.
		if err := n.CheckReadiness(); err != nil {
			if isSoftRuntimeErr(err) {
				recordSoft(fmt.Errorf("apply %q: %w (%w)", n.ID(), err, ErrNotReady))
				continue
			}
			recordHard(fmt.Errorf("apply %q: %w", n.ID(), err))
			continue
		}
		// Node reached a terminal ready state — unblock its dependents.
		ready[n.ID()] = true
	}
	// A hard error dominates any soft signal — errors.Join preserves errors.Is
	// for each joined error, and none of the hard errors carry ErrNotReady, so
	// the caller classifies the result as hard (degraded) rather than requeue.
	if len(hardErrs) > 0 {
		return result, errors.Join(hardErrs...)
	}
	return result, firstSoft
}

func (s *Simple) applyNodeByKind(
	ctx context.Context,
	rt *runtime.Runtime,
	w watchrouter.Watcher,
	n *runtime.Node,
	desired []*unstructured.Unstructured,
	result *ApplyResult,
) (softErr error, hardErr error) {
	switch n.Kind() {
	case compiler.NodeKindDef:
		// Def nodes have no cluster I/O — no managed-resource entries.
		n.SetObserved(desired, desired)
		publishScope(rt, n, n.Observed())
	case compiler.NodeKindTemplate:
		applied, err := s.applyTemplate(ctx, rt, w, n, desired)
		// Record whatever landed before any error so tracking never
		// loses a resource that actually reached the cluster.
		result.Applied = append(result.Applied, applied...)
		if err != nil {
			// A dynamic-GVK template whose target CRD isn't installed
			// or discoverable yet: identities for this node are
			// uncertain this cycle, so preserve previous entries via
			// Unresolved and requeue. The SchemaWatcher's dynamic set
			// re-enqueues the Graph when the CRD lands.
			if errors.Is(err, errSchemaNotReady) {
				result.Unresolved = append(result.Unresolved, n.ID())
				return fmt.Errorf("apply %q: %w (%w)", n.ID(), err, ErrNotReady), nil
			}
			// A terminating live object (ResourceDeletingError) or a
			// tolerated per-item collection failure surfaces as soft
			// not-ready: record Unresolved so prune is withheld, remember
			// the (possibly distinguishable) error, and continue so the
			// node never reaches ready — gating its dependents — and its
			// downstream watches still register. ResourceDeletingError
			// satisfies errors.Is(err, ErrNotReady).
			if errors.Is(err, ErrNotReady) {
				result.Unresolved = append(result.Unresolved, n.ID())
				return fmt.Errorf("apply %q: %w", n.ID(), err), nil
			}
			return nil, fmt.Errorf("apply %q: %w", n.ID(), err)
		}
		n.SetObserved(desired, desired)
		publishScope(rt, n, n.Observed())
	case compiler.NodeKindRef:
		var observed []*unstructured.Unstructured
		var err error
		isColl := n.IsCollection()
		if isColl {
			// A selector externalRef reads a read-only COLLECTION of
			// external objects by label selector.
			observed, err = s.applyRefCollection(ctx, w, n, desired)
		} else {
			observed, err = s.applyRef(ctx, w, rt, n, desired)
		}
		if err != nil {
			// A referenced object that isn't in the cluster yet is a
			// soft condition: it may be applied separately or created
			// later. Its identity is not ours to prune (read-only), so
			// record it Unresolved and requeue instead of failing hard.
			// A dynamic-GVK ref whose target CRD isn't installed yet
			// (errSchemaNotReady, from mappingFor) is soft too: the
			// SchemaWatcher re-enqueues the Graph when the CRD lands.
			if errors.Is(err, errSchemaNotReady) {
				result.Unresolved = append(result.Unresolved, n.ID())
				return fmt.Errorf("apply %q (ref): %w (%w)", n.ID(), err, ErrNotReady), nil
			}
			if errors.Is(err, ErrNotReady) || isSoftRuntimeErr(err) {
				result.Unresolved = append(result.Unresolved, n.ID())
				return fmt.Errorf("apply %q (ref): %w", n.ID(), err), nil
			}
			return nil, fmt.Errorf("apply %q (ref): %w", n.ID(), err)
		}
		// Read-only: publish the live object(s) so dependents resolve, but
		// never append to result.Applied — kro must not own, prune, or
		// delete a resource it only reads. For a selector collection the
		// desired set is a single projected ExternalRef (the selector
		// object), which must NOT be intersected against the observed list
		// of matched objects, so SetObserved is called with a nil desired
		// to preserve the list verbatim. publishScope emits a []any because
		// the node IsCollection.
		if isColl {
			n.SetObserved(observed, nil)
		} else {
			n.SetObserved(observed, desired)
		}
		publishScope(rt, n, n.Observed())
	case compiler.NodeKindPatch:
		contributions, err := s.applyPatch(ctx, rt, w, n, desired)
		// Record whatever landed before any error so tracking never loses a
		// contribution that actually reached the cluster (a forEach patch may
		// fail partway through).
		result.Contributions = append(result.Contributions, contributions...)
		if err != nil {
			// A dynamic-GVK patch whose target CRD isn't installed yet,
			// an absent target, a terminating target, or a field-manager conflict
			// are all soft: record Unresolved so nothing is pruned and requeue.
			if errors.Is(err, errSchemaNotReady) || errors.Is(err, ErrNotReady) || isSoftRuntimeErr(err) {
				result.Unresolved = append(result.Unresolved, n.ID())
				return fmt.Errorf("apply %q (patch): %w", n.ID(), err), nil
			}
			return nil, fmt.Errorf("apply %q (patch): %w", n.ID(), err)
		}
		// A patch publishes no value into scope; record observed so an
		// optional readyWhen can still evaluate against the node.
		n.SetObserved(desired, desired)
	default:
		return nil, fmt.Errorf("apply %q: unknown kind %v", n.ID(), n.Kind())
	}
	return nil, nil
}

// firstUnreadyDep returns the first direct dependency of n that has not
// reached a ready state this cycle, and whether such a dependency exists.
func firstUnreadyDep(n *runtime.Node, ready map[string]bool) (string, bool) {
	return n.FirstUnreadyDep(ready)
}

// managedResourceFrom builds a ManagedResource pointer from a node and
// its post-apply unstructured object. The UID is captured from the SSA
// response so the Reconciler can use it as a delete precondition later.
func managedResourceFrom(n *runtime.Node, obj *unstructured.Unstructured) expv1alpha1.ManagedResource {
	gvk := obj.GroupVersionKind()
	return expv1alpha1.ManagedResource{
		NodeID:     n.ID(),
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Namespace:  obj.GetNamespace(),
		Name:       obj.GetName(),
		UID:        string(obj.GetUID()),
	}
}

// isSoftRuntimeErr classifies a runtime-package error as a retryable
// "cluster hasn't converged yet" signal versus a hard error. Both
// ErrDataPending and ErrWaitingForReadiness mean "try again later" —
// the executor records them and continues so the rest of the graph
// still gets its watches declared.
func isSoftRuntimeErr(err error) bool {
	return errors.Is(err, runtime.ErrDataPending) || errors.Is(err, runtime.ErrWaitingForReadiness)
}

// publishScope writes the supplied objects back to the runtime scope.
// Collection nodes get a []any so downstream CEL list functions work;
// singletons get the lone object map.
func publishScope(rt *runtime.Runtime, n *runtime.Node, objs []*unstructured.Unstructured) {
	if n.IsCollection() {
		list := make([]any, 0, len(objs))
		for _, obj := range objs {
			list = append(list, obj.Object)
		}
		rt.Set(n.ID(), list)
		return
	}
	if len(objs) == 0 {
		return
	}
	rt.Set(n.ID(), objs[0].Object)
}

// Delete removes resources in reverse of the supplied slice order so
// dependents go before dependencies. Identity comes from the persisted
// ManagedResources list — no re-resolve of the current spec, so a
// Graph whose templates were renamed or whose forEach shrunk between
// apply and delete still gets every prior resource removed.
//
// Legitimate managed resources always carry a UID captured from SSA.
// Refuse to delete any entry with an empty UID to close the forged/UID-less
// prune vector where a user-forged status entry could delete arbitrary resources.
// NotFound and "already deleted by something else" are tolerated.
func (s *Simple) Delete(ctx context.Context, resources []expv1alpha1.ManagedResource) error {
	var errs []error
	for _, r := range slices.Backward(resources) {
		// Legitimate managed resources always carry a UID captured from SSA.
		// Refuse to delete any resource without a UID to close the forged/UID-less prune vector.
		if r.UID == "" {
			continue
		}

		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion(r.APIVersion)
		obj.SetKind(r.Kind)
		obj.SetNamespace(r.Namespace)
		obj.SetName(r.Name)

		uid := types.UID(r.UID)
		opts := []client.DeleteOption{
			&client.DeleteOptions{
				Preconditions: &metav1.Preconditions{UID: &uid},
			},
		}

		if err := s.Client.Delete(ctx, obj, opts...); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			// UID-precondition mismatch surfaces as Conflict — the
			// resource we tracked is gone and a different object now
			// occupies its identity. Not our problem; skip.
			if apierrors.IsConflict(err) {
				continue
			}
			// Accumulate and keep going: one denied/failed delete (e.g. an
			// impersonated SA lacking delete RBAC on a single target) must not
			// strand every remaining managed resource in the inventory. The
			// aggregate error is returned after the whole inventory is visited.
			errs = append(errs, fmt.Errorf("delete %s/%s %s: %w", r.APIVersion, r.Kind, refName(r), err))
		}
	}
	return errors.Join(errs...)
}

func refName(r expv1alpha1.ManagedResource) string {
	if r.Namespace == "" {
		return r.Name
	}
	return r.Namespace + "/" + r.Name
}

// errSchemaNotReady signals that a dynamic-GVK template resolved to a GVK
// the cluster can't yet map to a resource (the CRD isn't installed or the
// REST mapper hasn't discovered it). It is an internal soft signal: Apply
// maps it to Unresolved + ErrNotReady so the reconcile requeues.
var errSchemaNotReady = errors.New("executor: target GVK not yet known to the cluster")

// applyTemplate renders, watches, and SSA-applies every object a Template
// node produced. GVR and REST scope are resolved per object — from the
// compiled spec for static nodes, from the live REST mapper for dynamic-GVK
// nodes. Mappings for all objects are resolved up front so a dynamic node
// whose CRD isn't installed yet fails (errSchemaNotReady) before any
// partial apply. Returns the resources that actually landed, even on a
// later hard error, so the caller never loses tracking for them.
//
// Before SSA-applying each object the live object is fetched: if it exists
// with a deletionTimestamp the
// node is held soft not-ready via ResourceDeletingError so its dependents
// gate and the reconcile requeues without recreating a resource that is
// still terminating. External refs/collections are exempt (they are
// read-only and handled by applyRef/applyRefCollection, not here).
//
// Collection nodes apply each item INDEPENDENTLY and tolerate per-item SSA
// failures: a failure to
// UPDATE an already-present item (e.g. an immutable field) does not abort
// the node — the live object is recorded as applied so tracking/prune stay
// correct and the node can still converge. A failure to CREATE an item that
// is not yet present marks the node soft not-ready so downstream gates and
// the reconcile requeues. A scalar node still returns an SSA error as a hard
// error, unchanged.
func (s *Simple) applyTemplate(ctx context.Context, rt *runtime.Runtime, w watchrouter.Watcher, n *runtime.Node, desired []*unstructured.Unstructured) ([]expv1alpha1.ManagedResource, error) {
	mappings, err := s.buildMappings(n, desired)
	if err != nil {
		return nil, err
	}
	if n.IsCollection() {
		return s.applyCollectionTemplate(ctx, w, rt, n, desired, mappings)
	}
	return s.applyScalarTemplate(ctx, w, rt, n, desired, mappings)
}

// applyMapping is the resolved GVR + REST scope for one rendered object.
type applyMapping struct {
	gvr        schema.GroupVersionResource
	namespaced bool
}

// buildMappings resolves the GVR/scope for every object up front so a
// dynamic-GVK node whose CRD isn't installed yet fails before any partial
// apply.
func (s *Simple) buildMappings(n *runtime.Node, desired []*unstructured.Unstructured) ([]applyMapping, error) {
	mappings := make([]applyMapping, len(desired))
	for i, obj := range desired {
		gvr, namespaced, err := s.mappingFor(n, obj)
		if err != nil {
			return nil, err
		}
		mappings[i] = applyMapping{gvr: gvr, namespaced: namespaced}
	}
	return mappings, nil
}

// prepareItem namespace-defaults, validates the namespace, and stamps kro
// metadata + per-instance labels onto obj before apply. Shared by the scalar
// and collection paths.
func (s *Simple) prepareItem(rt *runtime.Runtime, n *runtime.Node, obj *unstructured.Unstructured, i, size int, m applyMapping) error {
	s.defaultNamespace(rt, m.namespaced, obj)
	if m.namespaced && obj.GetNamespace() == "" {
		return fmt.Errorf("node %q: namespaced resource %s/%s must set metadata.namespace when the instance is cluster-scoped", n.ID(), obj.GetKind(), obj.GetName())
	}
	// Pre-write duplicate-identity guard: claim this object's identity BEFORE
	// the SSA write. If another node already claimed it this walk, refuse now so
	// the second node cannot clobber the first's object (RGD path) or force-
	// reclaim its fields forever (standalone-Graph path). The identity key
	// matches validateAppliedIdentities (apiVersion/kind/namespace/name); the
	// owner is the frame-qualified node path so a legitimate re-claim by the same
	// node is allowed while a cross-node collision is a hard error.
	if s.identityClaims != nil {
		gvk := obj.GroupVersionKind()
		identityKey := fmt.Sprintf("%s/%s/%s/%s", gvk.GroupVersion().String(), gvk.Kind, obj.GetNamespace(), obj.GetName())
		if err := s.identityClaims.claim(identityKey, s.qualifiedPath(n.ID())); err != nil {
			return err
		}
	}
	s.stampKROMeta(rt, n, obj, i, size)
	if s.LabelInjector != nil {
		s.LabelInjector(obj)
	}
	return nil
}

// applySetConflict returns an ApplySetConflictError when the live object
// already belongs to a different ApplySet, so kro does not overwrite a resource
// another applyset owns. A nil current (absent object) never conflicts.
func applySetConflict(current, obj *unstructured.Unstructured) error {
	if current == nil {
		return nil
	}
	currentApplySetID := current.GetLabels()[applyset.ApplysetPartOfLabel]
	desiredApplySetID := obj.GetLabels()[applyset.ApplysetPartOfLabel]
	if currentApplySetID != "" && currentApplySetID != desiredApplySetID {
		return &applyset.ApplySetConflictError{
			ResourceName:      obj.GetName(),
			ResourceNamespace: obj.GetNamespace(),
			ResourceGVK:       obj.GroupVersionKind().String(),
			CurrentApplySetID: currentApplySetID,
			DesiredApplySetID: desiredApplySetID,
		}
	}
	return nil
}

// applyScalarTemplate applies a non-collection Template: one object, per-item
// watch, and an SSA failure is a hard error (unchanged behavior).
func (s *Simple) applyScalarTemplate(ctx context.Context, w watchrouter.Watcher, rt *runtime.Runtime, n *runtime.Node, desired []*unstructured.Unstructured, mappings []applyMapping) ([]expv1alpha1.ManagedResource, error) {
	applied := make([]expv1alpha1.ManagedResource, 0, len(desired))
	size := len(desired)
	for i, obj := range desired {
		if err := s.prepareItem(rt, n, obj, i, size, mappings[i]); err != nil {
			return applied, err
		}
		if err := s.watchObject(ctx, w, s.qualifiedPath(n.ID()), mappings[i].gvr, obj); err != nil {
			// A failure to register the drift watch must not abort the apply:
			// the object is still applied, only drift re-enqueue is lost for it
			// (the base pre-drift behavior). Log and continue.
			log.FromContext(ctx).Info("drift watch registration failed; applying without drift detection for this node",
				"node", s.qualifiedPath(n.ID()), "gvr", mappings[i].gvr.String(), "err", err.Error())
		}

		// Fetch the live object: a hard GET error aborts,
		// NotFound means the object is absent (current == nil).
		current, err := s.getLive(ctx, obj)
		if err != nil {
			return applied, err
		}
		// Terminating: do not re-apply while the live object is being deleted.
		if current != nil && current.GetDeletionTimestamp() != nil {
			return applied, &ResourceDeletingError{NodeID: s.qualifiedPath(n.ID()), Namespace: obj.GetNamespace(), Name: obj.GetName()}
		}
		if err := applySetConflict(current, obj); err != nil {
			return applied, err
		}
		// Cross-engine guard (RGD/instance path only): a live object owned by a
		// standalone Graph's template manager must not be force-adopted under the
		// shared field manager. Surfaced soft not-ready, matching the Graph path's
		// peer-conflict treatment so the reconcile backs off instead of flapping.
		if !s.ConflictDetection && ownedByGraphTemplate(current) {
			return applied, fmt.Errorf("template %s %q owned by a foreign kro Graph: %w (%w)",
				obj.GetKind(), client.ObjectKeyFromObject(obj), ErrFieldManagerConflict, ErrNotReady)
		}
		// Cross-Graph adoption guard (standalone Graph path only): refuse to
		// ADOPT/MANAGE a live object already owned by a PEER Graph's template
		// manager BEFORE we apply. applyTemplateObject only rejects a peer when
		// the unforced SSA raises an actual field-level 409, i.e. when the two
		// Graphs contend over the SAME field; if their field sets are DISJOINT
		// there is no 409 and the second Graph would silently adopt the object,
		// record it in inventory, and later delete/mutate a peer's resource.
		// Surfaced SOFT not-ready (ErrFieldManagerConflict wraps ErrNotReady),
		// uniform with both the same-field peer conflict below and the RGD path's
		// ownedByGraphTemplate guard: the reconcile backs off instead of flapping,
		// and the object is NOT appended to `applied` (this returns before the
		// append), so it never enters inventory and Delete never sees it. The
		// patch: node stays the intentional mechanism for touching an object this
		// Graph doesn't own.
		if s.ConflictDetection && ownedByForeignGraphTemplate(current, templateFieldManager(rt.Graph().GetUID())) {
			return applied, fmt.Errorf("template %s %q owned by a peer kro Graph's template manager; refusing to co-manage: %w (%w)",
				obj.GetKind(), client.ObjectKeyFromObject(obj), ErrFieldManagerConflict, ErrNotReady)
		}
		if err := s.applyTemplateObject(ctx, obj, rt.Graph().GetUID()); err != nil {
			// A peer-Graph field conflict is soft not-ready (wrapped with
			// ErrNotReady) so the node gates its dependents and the reconcile
			// backs off rather than flip-flopping the contested field. Any
			// other scalar SSA failure stays a hard error, unchanged.
			if errors.Is(err, ErrFieldManagerConflict) {
				return applied, fmt.Errorf("%w (%w)", err, ErrNotReady)
			}
			return applied, err
		}
		// SSA returned UID + server-managed fields on obj. Record the
		// identity AFTER apply succeeded so we never advertise tracking
		// for resources that didn't actually land.
		applied = append(applied, managedResourceFrom(n, obj))
	}
	return applied, nil
}

// collectionApplyState accumulates the results of a parallel collection apply.
// All mutation goes through its methods under the mutex; read the fields only
// after the errgroup has been waited on.
type collectionApplyState struct {
	mu           sync.Mutex
	results      []*expv1alpha1.ManagedResource
	hardErrors   []error
	deletingErr  *ResourceDeletingError
	itemFailures []error
}

func (st *collectionApplyState) recordApplied(i int, mr expv1alpha1.ManagedResource) {
	st.mu.Lock()
	st.results[i] = &mr
	st.mu.Unlock()
}

// classifyRejection turns a tolerated collection-update rejection into a short
// operator-facing reason and a permanent/transient flag. A permanent rejection
// (Invalid/BadRequest) cannot succeed by retrying the same payload; an Invalid
// whose status causes name an immutable field is reported specifically so the
// operator sees WHY the desired change never landed. Transient causes
// (Conflict, timeouts, throttling, unavailability) are retried on a later
// reconcile, so they are flagged non-permanent. This is best-effort: a webhook
// Forbidden or a CEL-validation Invalid that depends on other mutable cluster
// state cannot be perfectly classified by error code, which is exactly why the
// node converges and merely SURFACES the rejection rather than gating on it.
func classifyRejection(err error) (reason string, permanent bool) {
	switch {
	case apierrors.IsInvalid(err):
		if isImmutableFieldError(err) {
			return "field immutable", true
		}
		return "invalid request", true
	case apierrors.IsBadRequest(err):
		return "invalid request", true
	case apierrors.IsConflict(err):
		return "field-manager conflict, will retry", false
	case apierrors.IsTooManyRequests(err):
		return "throttled, will retry", false
	case apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) || apierrors.IsServiceUnavailable(err) || apierrors.IsInternalError(err):
		return "transient server error, will retry", false
	default:
		// Unknown cause: treat as transient (a later reconcile re-attempts the
		// update anyway) but don't claim permanence we can't prove.
		return "rejected, will retry", false
	}
}

// isImmutableFieldError reports whether an Invalid error's status causes name an
// immutable-field violation (the apiserver phrasing is "field is immutable" or
// "may not be changed"). Used only to enrich the operator-facing reason.
func isImmutableFieldError(err error) bool {
	status := apierrors.APIStatus(nil)
	if !errors.As(err, &status) {
		return false
	}
	details := status.Status().Details
	if details == nil {
		return false
	}
	for _, cause := range details.Causes {
		msg := strings.ToLower(cause.Message)
		if strings.Contains(msg, "immutable") || strings.Contains(msg, "may not be changed") || strings.Contains(msg, "cannot be changed") {
			return true
		}
	}
	return false
}

// recordUpdateRejected records a tolerated per-item UPDATE failure on an
// already-present object: the live identity is tracked and the desired slot is
// replaced with the live object so downstream scope sees what actually landed.
func (st *collectionApplyState) recordUpdateRejected(i int, mr expv1alpha1.ManagedResource, desired []*unstructured.Unstructured, current *unstructured.Unstructured) {
	st.mu.Lock()
	st.results[i] = &mr
	desired[i] = current
	st.mu.Unlock()
}

func (st *collectionApplyState) recordDeleting(de *ResourceDeletingError) {
	st.mu.Lock()
	if st.deletingErr == nil {
		st.deletingErr = de
	}
	st.mu.Unlock()
}

func (st *collectionApplyState) deletingError() *ResourceDeletingError {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.deletingErr
}

func (st *collectionApplyState) recordHardError(i int, err error) {
	st.mu.Lock()
	st.hardErrors[i] = err
	st.mu.Unlock()
}

func (st *collectionApplyState) recordFailure(i int, err error) {
	st.mu.Lock()
	st.itemFailures[i] = err
	st.mu.Unlock()
}

func (st *collectionApplyState) appliedResources() []expv1alpha1.ManagedResource {
	st.mu.Lock()
	defer st.mu.Unlock()
	applied := make([]expv1alpha1.ManagedResource, 0, len(st.results))
	for _, r := range st.results {
		if r != nil {
			applied = append(applied, *r)
		}
	}
	return applied
}

func nonNilErrors(errs []error) []error {
	return slices.DeleteFunc(slices.Clone(errs), func(e error) bool { return e == nil })
}

func (st *collectionApplyState) hardError() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	return errors.Join(nonNilErrors(st.hardErrors)...)
}

func (st *collectionApplyState) softError(nodeID string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	errs := nonNilErrors(st.itemFailures)
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("collection %q: %d item(s) failed to apply: %w (%w)", nodeID, len(errs), errors.Join(errs...), ErrNotReady)
}

// applyCollectionTemplate applies a collection Template with bounded
// parallelism. It registers ONE selector watch for the whole node up front,
// then applies each item independently, tolerating per-item failures: a
// rejected UPDATE on an already-present item records the live identity and
// converges; a failed CREATE holds the node soft not-ready; a terminating item
// gates the node. Hard errors (bad GET, namespace, applyset conflict, permanent
// validation rejection) abort and are deterministically joined by item index.
func (s *Simple) applyCollectionTemplate(ctx context.Context, w watchrouter.Watcher, rt *runtime.Runtime, n *runtime.Node, desired []*unstructured.Unstructured, mappings []applyMapping) ([]expv1alpha1.ManagedResource, error) {
	if len(desired) == 0 {
		return []expv1alpha1.ManagedResource{}, nil
	}

	// Collection nodes register ONE selector-based watch for the whole
	// node (keyed by NodeID, matching every item by label) instead of N
	// scalar watches — the coordinator keys state by NodeID, so per-item
	// scalar watches would collapse to only the last item. Registered
	// once up front before spawning parallel apply goroutines.
	//
	// The watch namespace is scoped to a single namespace when every item in
	// the (fully-resolved) collection lands there, and left empty (all-
	// namespaces) only when the collection genuinely spans namespaces. A
	// namespaced watch is cheaper than an all-namespaces one, so this avoids
	// the broad watch for the common single-namespace case.
	ns := s.collectionWatchNamespace(rt, desired, mappings)
	// Register one selector watch per DISTINCT GVR in the collection. A static
	// collection has a single GVR (mappings all equal), but a dynamic-GVK
	// collection can render items across several GVRs — registering only
	// mappings[0] would leave every other rendered type without drift detection.
	// The coordinator keys watch state by (NodeID, GVR), so one selector watch
	// per GVR under the same node is correct and non-colliding; a representative
	// sample object per GVR carries the labels the selector matches.
	for _, wm := range distinctWatchMappings(desired, mappings) {
		if err := s.watchCollection(ctx, w, rt, n, wm.gvr, wm.sample, ns); err != nil {
			// A failure to register a collection drift watch must not abort the
			// apply: the collection is still applied, only drift re-enqueue is lost
			// for that GVR. Log and continue.
			log.FromContext(ctx).Info("collection drift watch registration failed; applying without drift detection for this GVR",
				"node", s.qualifiedPath(n.ID()), "gvr", wm.gvr.String(), "err", err.Error())
		}
	}

	bound := defaultApplyConcurrency
	if s.ApplyConcurrency > 0 {
		bound = s.ApplyConcurrency
	}
	if bound > len(desired) {
		bound = len(desired)
	}

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(bound)

	size := len(desired)
	st := &collectionApplyState{
		results:      make([]*expv1alpha1.ManagedResource, len(desired)),
		hardErrors:   make([]error, len(desired)),
		itemFailures: make([]error, len(desired)),
	}
	for i := range desired {
		g.Go(func() error {
			return s.applyCollectionItem(gCtx, rt, n, desired, i, size, mappings[i], st)
		})
	}

	if err := g.Wait(); err != nil {
		return st.appliedResources(), err
	}
	// Hard errors collected across items are joined deterministically by index.
	if hardErr := st.hardError(); hardErr != nil {
		return st.appliedResources(), hardErr
	}
	// A terminating item takes priority: surface the ResourceDeleting signal
	// so the reconciler marks the ResourcesReady condition accordingly.
	if deletingErr := st.deletingError(); deletingErr != nil {
		return st.appliedResources(), deletingErr
	}
	// Any create failure holds the collection soft not-ready so downstream
	// gates and the reconcile requeues (never a hard abort).
	if softErr := st.softError(n.ID()); softErr != nil {
		return st.appliedResources(), softErr
	}
	return st.appliedResources(), nil
}

// applyCollectionItem applies one collection member. It runs concurrently with
// its siblings; all shared results flow through st under its mutex.
func (s *Simple) applyCollectionItem(ctx context.Context, rt *runtime.Runtime, n *runtime.Node, desired []*unstructured.Unstructured, i, size int, m applyMapping, st *collectionApplyState) error {
	obj := desired[i]
	if err := s.prepareItem(rt, n, obj, i, size, m); err != nil {
		st.recordHardError(i, err)
		return nil
	}

	// Fetch the live object: a hard GET error aborts,
	// NotFound means the object is absent (current == nil).
	current, err := s.getLive(ctx, obj)
	if err != nil {
		st.recordHardError(i, err)
		return nil
	}
	// Terminating: a terminating item gates the whole node, but keep
	// applying siblings so their watches/identities are recorded.
	if current != nil && current.GetDeletionTimestamp() != nil {
		st.recordDeleting(&ResourceDeletingError{NodeID: s.qualifiedPath(n.ID()), Namespace: obj.GetNamespace(), Name: obj.GetName()})
		return nil
	}
	if err := applySetConflict(current, obj); err != nil {
		st.recordHardError(i, err)
		return nil
	}
	// Cross-engine guard (RGD/instance path only): a live object owned by a
	// standalone Graph's template manager must not be force-adopted under the
	// shared field manager. Held soft not-ready via recordFailure, matching the
	// Graph path's peer-conflict treatment.
	if !s.ConflictDetection && ownedByGraphTemplate(current) {
		st.recordFailure(i, fmt.Errorf("item %s/%s owned by a foreign kro Graph: %w",
			obj.GetNamespace(), obj.GetName(), ErrFieldManagerConflict))
		return nil
	}
	// Cross-Graph adoption guard (standalone Graph path only): refuse to
	// ADOPT/MANAGE an item already owned by a PEER Graph's template manager
	// BEFORE we apply. When the two Graphs' field sets are DISJOINT the unforced
	// SSA raises no 409, so applyTemplateObject would silently adopt the peer's
	// object, record it in inventory, and later delete/mutate it. Surfaced SOFT
	// not-ready via recordFailure (ErrFieldManagerConflict wraps ErrNotReady),
	// uniform with the same-field peer conflict below and the RGD path's
	// ownedByGraphTemplate guard: the item is NOT appended to results, so it
	// never enters inventory. It is per-item and does not abort siblings.
	if s.ConflictDetection && ownedByForeignGraphTemplate(current, templateFieldManager(rt.Graph().GetUID())) {
		st.recordFailure(i, fmt.Errorf("item %s/%s owned by a peer kro Graph's template manager; refusing to co-manage: %w",
			obj.GetNamespace(), obj.GetName(), ErrFieldManagerConflict))
		return nil
	}

	if err := s.applyTemplateObject(ctx, obj, rt.Graph().GetUID()); err != nil {
		// A peer-Graph field conflict is never tolerated as an update-rejection:
		// record it as a soft failure so the collection is held not-ready and the
		// reconcile backs off instead of flip-flopping the contested field.
		if errors.Is(err, ErrFieldManagerConflict) {
			st.recordFailure(i, fmt.Errorf("item %s/%s: %w", obj.GetNamespace(), obj.GetName(), err))
			return nil
		}
		if current != nil {
			// The object already exists; only the UPDATE was rejected. This is
			// tolerated BY DESIGN, including permanent rejections such as a
			// Kubernetes immutable-field update (`Forbidden: pod updates may not
			// change fields ...`): the object is present in the cluster, so record
			// the live identity and let the collection converge rather than block
			// the node forever on an unfixable update. (Integration coverage:
			// collection_test.go deep-chaining scale up/down relies on this.)
			//
			// The rejection is NOT escalated to a hard error or a soft not-ready
			// (that would wedge the node on an unfixable update), but it must not
			// be fully SILENT either: a desired change did not land while the node
			// still converges. Emit a warning to the controller log AND, when the
			// caller wired OnToleratedRejection, an observational signal (the
			// instance controller records a Warning event) classifying WHY — so a
			// stale live object is diagnosable in `kubectl describe`, not just in
			// logs. The signal is observational ONLY: it never touches readiness
			// gating or requeue, or the anti-wedge tolerance would be lost.
			reason, permanent := classifyRejection(err)
			log.FromContext(ctx).Info("collection item update rejected; keeping live object and converging (desired change did not land)",
				"node", s.qualifiedPath(n.ID()),
				"object", obj.GetNamespace()+"/"+obj.GetName(),
				"gvk", obj.GroupVersionKind().String(),
				"reason", reason,
				"permanent", permanent,
				"cause", err.Error())
			if s.OnToleratedRejection != nil {
				gvk := obj.GroupVersionKind()
				s.OnToleratedRejection(ToleratedRejection{
					NodeID:     s.qualifiedPath(n.ID()),
					APIVersion: gvk.GroupVersion().String(),
					Kind:       gvk.Kind,
					Namespace:  obj.GetNamespace(),
					Name:       obj.GetName(),
					Reason:     reason,
					Permanent:  permanent,
					Cause:      err.Error(),
				})
			}
			st.recordUpdateRejected(i, managedResourceFrom(n, current), desired, current)
			return nil
		}
		// The object does not exist and CREATE failed. A genuinely malformed
		// object (Invalid / BadRequest) can never succeed — surface it as a hard
		// error so the node fails fast instead of requeuing forever on an
		// unfixable create. Transient/again-later causes (quota, throttling,
		// RBAC being provisioned, generic errors) stay soft not-ready so the
		// reconcile retries. Siblings still apply either way.
		if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
			st.recordHardError(i, fmt.Errorf("item %s/%s: %w", obj.GetNamespace(), obj.GetName(), err))
			return nil
		}
		st.recordFailure(i, fmt.Errorf("item %s/%s: %w", obj.GetNamespace(), obj.GetName(), err))
		return nil
	}
	// SSA returned UID + server-managed fields on obj. Record the
	// identity AFTER apply succeeded so we never advertise tracking
	// for resources that didn't actually land.
	st.recordApplied(i, managedResourceFrom(n, obj))
	return nil
}

// getLive fetches the current cluster state of obj by GVK/namespace/name.
// A NotFound is reported as (nil, nil) — the object is simply absent. Any
// other error is returned so the caller can abort. Used to detect a
// terminating object (deletionTimestamp) and to distinguish create-vs-update
// failures for collection per-item apply tolerance.
func (s *Simple) getLive(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(obj.GroupVersionKind())
	key := client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	if err := s.Client.Get(ctx, key, live); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get current state for %s %q: %w", obj.GetKind(), key, err)
	}
	return live, nil
}

// qualifiedPath returns the fully-qualified, human-readable node path for a
// local node ID, joining the executor's frame prefix with '/' (e.g. "subA/res"
// inside subgraph "subA", "res" at the root). This is the free-form rendering
// used for the coordinator watch key, the ManagedResource store, and the
// node-path annotation — it is never used as a label value.
func (s *Simple) qualifiedPath(id string) string {
	return s.nodePrefix + id
}

// nodeIDToken returns a bounded, label-safe rendering of a node's qualified
// path for the kro.run/node-id label value and the collection watch selector.
//
// Node IDs are strictly alphanumeric (^[A-Za-z][A-Za-z0-9]*$), so '.' is an
// unambiguous, reversible frame separator: the '.'-joined path (e.g.
// "subA.res") is a valid label value and round-trips to the '/'-form. At the
// root the token is just the bare node ID, preserving the documented
// `kubectl get -l kro.run/node-id=<id>` query for top-level nodes.
//
// When the '.'-joined path would exceed the 63-char label-value limit (deep or
// long-named nesting) it is replaced by a stable, collision-resistant hash so
// the label stays valid at any depth. The full readable path is always
// preserved in the node-path annotation regardless, so a hashed label never
// costs debuggability. The selector is built from this same function, so it
// matches the stamped label by construction.
func (s *Simple) nodeIDToken(id string) string {
	dotted := strings.ReplaceAll(s.qualifiedPath(id), "/", ".")
	if len(dotted) <= validation.LabelValueMaxLength {
		return dotted
	}
	// Fallback: "h-<40 hex>" of the '/'-form. The leading letter keeps the
	// value a valid label (must start alphanumeric) and marks it as hashed.
	sum := sha256.Sum256([]byte(s.qualifiedPath(id)))
	return "h-" + hex.EncodeToString(sum[:20])
}

// stampKROMeta stamps the identity metadata kro relies on: the
// kro.run/node-id label (a bounded, label-safe token used by selectors and
// managed-resource discovery), the internal.kro.run/node-path annotation (the
// full human-readable qualified path), and the internal.kro.run/apply-order
// annotation (the reverse-topological deletion wave read by the instance
// deletion path). For collection items it also stamps the collection-index /
// collection-size labels (index within the expansion, total item count).
// Per-instance labels are added separately by the executor's LabelInjector.
func (s *Simple) stampKROMeta(rt *runtime.Runtime, n *runtime.Node, obj *unstructured.Unstructured, index, size int) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = make(map[string]string, 3)
	}
	labels[metadata.NodeIDLabel] = s.nodeIDToken(n.ID())
	if n.IsCollection() {
		labels[metadata.CollectionIndexLabel] = strconv.Itoa(index)
		labels[metadata.CollectionSizeLabel] = strconv.Itoa(size)
		// A COLLECTION node's drift watch is a selector watch keyed on
		// {node-id, instance-id} (watchCollection). On the RGD/instance path the
		// LabelInjector stamps instance-id (the instance UID); on the standalone
		// Graph path there is no injector, so stamp the Graph's own UID here so
		// the applied items match the selector — otherwise drift/deletion events
		// never match and go unobserved. Only for collections: a SCALAR template
		// uses a name-based watch (watchObject) that needs no instance-id, and
		// stamping it there would make two Graphs legitimately sharing a scalar
		// object (writing disjoint fields) conflict on the instance-id label.
		// Only set the fallback when no injector will supply it, and never clobber
		// an injector-provided value.
		if _, ok := labels[metadata.InstanceIDLabel]; s.LabelInjector == nil && !ok {
			if uid := string(rt.Graph().GetUID()); uid != "" {
				labels[metadata.InstanceIDLabel] = uid
			}
		}
	}
	obj.SetLabels(labels)

	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string, 2)
	}
	annotations[metadata.NodePathAnnotation] = s.qualifiedPath(n.ID())
	if order, ok := rt.ApplyOrder(n.ID()); ok {
		annotations[metadata.ApplyOrderAnnotation] = strconv.Itoa(order)
	}
	obj.SetAnnotations(annotations)
}

// applyRef reads the external resource a ref node points at and returns its
// live cluster state for publication into scope. A ref is READ-ONLY: kro
// registers a watch so the Graph re-reconciles when the referenced object
// changes, but never applies, owns, or prunes it. The caller therefore must
// NOT record the returned object in ApplyResult.Applied.
//
// A referenced object that doesn't exist yet is a soft condition, not a
// failure: it may be applied separately or created later, so we wrap
// ErrNotReady and let the reconciler requeue. The watch is registered before
// the read (and fires on create), so the Graph re-enqueues once it appears.
func (s *Simple) applyRef(ctx context.Context, w watchrouter.Watcher, rt *runtime.Runtime, n *runtime.Node, desired []*unstructured.Unstructured) ([]*unstructured.Unstructured, error) {
	// forEach is rejected on ref nodes at compile time, so Resolve produced
	// exactly one projected {apiVersion, kind, metadata} object.
	if len(desired) != 1 {
		return nil, fmt.Errorf("ref node resolved to %d objects, want 1", len(desired))
	}
	ref := desired[0]
	// Fill metadata.namespace from the Graph for namespaced kinds when the
	// ExternalRef left it empty — matches the "defaults to the instance's
	// namespace" contract on ExternalRefMetadata.
	//
	// Resolve the GVR + REST scope for the watch. Static refs use the
	// compile-time GVR; a dynamic-GVK ref has none, so mappingFor resolves the
	// rendered object's concrete GVK through the live REST mapper (returning
	// errSchemaNotReady when the target CRD isn't installed yet — soft
	// not-ready, propagated below via applyNodeByKind).
	gvr, namespaced, err := s.mappingFor(n, ref)
	if err != nil {
		return nil, err
	}
	s.defaultNamespace(rt, namespaced, ref)

	if err := s.watchObject(ctx, w, s.qualifiedPath(n.ID()), gvr, ref); err != nil {
		// A failure to register the drift watch must not abort the apply: the
		// live object is still read and published below, only drift re-enqueue
		// is lost. Log and continue.
		log.FromContext(ctx).Info("drift watch registration failed; reading ref without drift detection",
			"node", s.qualifiedPath(n.ID()), "gvr", gvr.String(), "err", err.Error())
	}

	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(ref.GroupVersionKind())
	key := client.ObjectKey{Namespace: ref.GetNamespace(), Name: ref.GetName()}
	if err := s.Client.Get(ctx, key, live); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("external ref %s %q not found: %w", ref.GetKind(), key, ErrNotReady)
		}
		return nil, fmt.Errorf("get external ref %s %q: %w", ref.GetKind(), key, err)
	}
	return []*unstructured.Unstructured{live}, nil
}

// applyRefCollection reads the read-only COLLECTION of external objects a
// selector externalRef points at and returns their live cluster state for
// publication into scope. It resolves the label selector off the rendered
// ExternalRef, registers ONE
// selector-based watch keyed by NodeID, lists the GVR by that selector, and
// publishes the matched objects. A selector ref is READ-ONLY: kro never applies,
// owns, or prunes the matched objects, so the caller must NOT record them in
// ApplyResult.Applied.
//
// Namespace handling deliberately skips defaultNamespace: an empty
// metadata.namespace for a namespaced GVR means "list across ALL namespaces",
// not "the instance's
// namespace". An empty selector lists everything. A List error is hard; an
// empty result is valid (an empty collection, treated as ready).
func (s *Simple) applyRefCollection(ctx context.Context, w watchrouter.Watcher, n *runtime.Node, desired []*unstructured.Unstructured) ([]*unstructured.Unstructured, error) {
	// forEach is rejected on ref nodes at translate time, so Resolve produced
	// exactly one projected {apiVersion, kind, metadata{selector,namespace?}}
	// object with CEL (incl. matchExpressions[].values[]) already evaluated.
	if len(desired) != 1 {
		return nil, fmt.Errorf("ref collection node resolved to %d objects, want 1", len(desired))
	}
	ref := desired[0]

	selector, err := refCollectionSelector(n.ID(), ref)
	if err != nil {
		return nil, err
	}

	// Resolve the GVR + REST scope. Static refs use the compile-time GVR; a
	// dynamic-GVK selector ref has none, so mappingFor resolves the rendered
	// object's concrete GVK (from its CEL-evaluated apiVersion/kind) through the
	// live REST mapper, returning errSchemaNotReady when the target CRD isn't
	// installed yet (soft not-ready, propagated by applyNodeByKind).
	gvr, namespaced, err := s.mappingFor(n, ref)
	if err != nil {
		return nil, err
	}

	// Namespace comes straight from the rendered ExternalRef. For a namespaced
	// GVR an empty namespace lists across all namespaces; cluster-scoped GVRs
	// are always list-all. No defaultNamespace: namespace normalization is
	// intentionally skipped for external collections.
	ns := ref.GetNamespace()
	if !namespaced {
		ns = ""
	}

	// Register the selector watch BEFORE the list so a matching object that
	// appears between the list and watch registration still re-enqueues the
	// Graph. The watch is keyed by NodeID with the user's label selector.
	// A failure to register the watch (or a denied access review) must not
	// abort: the collection is still listed and published below, only drift
	// re-enqueue is lost. Log and continue.
	if w != nil && !s.skipWatchForIdentity(ctx, gvr, ns) {
		if err := w.Watch(watchrouter.WatchRequest{
			NodeID:    s.qualifiedPath(n.ID()),
			GVR:       gvr,
			Namespace: ns,
			Selector:  selector,
		}); err != nil {
			log.FromContext(ctx).Info("external collection drift watch registration failed; reading without drift detection",
				"node", s.qualifiedPath(n.ID()), "gvr", gvr.String(), "err", err.Error())
		}
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(ref.GroupVersionKind())
	opts := []client.ListOption{client.MatchingLabelsSelector{Selector: selector}}
	if ns != "" {
		opts = append(opts, client.InNamespace(ns))
	}
	if err := s.Client.List(ctx, list, opts...); err != nil {
		return nil, fmt.Errorf("list external collection %q (%s): %w", n.ID(), gvr.String(), err)
	}

	items := make([]*unstructured.Unstructured, len(list.Items))
	for i := range list.Items {
		items[i] = &list.Items[i]
	}
	return items, nil
}

// refCollectionSelector extracts the label selector from a rendered
// external-collection ExternalRef. A missing or empty metadata.selector means
// "select everything" (labels.Everything).
func refCollectionSelector(id string, ref *unstructured.Unstructured) (labels.Selector, error) {
	selectorRaw, found, err := unstructured.NestedMap(ref.Object, "metadata", "selector")
	if err != nil || !found {
		return labels.Everything(), nil
	}
	ls := &metav1.LabelSelector{}
	if err := apimachineryruntime.DefaultUnstructuredConverter.FromUnstructured(selectorRaw, ls); err != nil {
		return nil, fmt.Errorf("convert selector for %q: %w", id, err)
	}
	selector, err := metav1.LabelSelectorAsSelector(ls)
	if err != nil {
		return nil, fmt.Errorf("invalid label selector for %q: %w", id, err)
	}
	return selector, nil
}

// applySubgraph runs a nested Graph node's child Program. The child Runtime
// is seeded with a snapshot of this scope so child expressions can capture
// (and shadow) parent values. The child's node outputs are published back to
// the parent scope as a map under the subgraph node's ID, making them
// addressable as ${nodeID.childNode.field}. Managed resources and unresolved
// NodeIDs from the child are returned with their IDs qualified by the
// subgraph node ID so the reconciler's tracking stays unambiguous across
// frames. The child executor also carries this frame prefix (child.nodePrefix),
// so the identity metadata it stamps deep in the apply — the coordinator watch
// key, the kro.run/node-id label/selector token, and the node-path annotation
// — is qualified consistently at the source. Contributions from nested patch
// nodes are propagated up to the parent result. The child's watches register
// against the same per-Graph Watcher, so drift on a nested resource
// re-enqueues the owning Graph.
func (s *Simple) applySubgraph(ctx context.Context, rt *runtime.Runtime, w watchrouter.Watcher, n *runtime.Node) ([]expv1alpha1.ManagedResource, []string, []Contribution, error) {
	sub := n.Spec().SubProgram
	if sub == nil {
		return nil, nil, nil, fmt.Errorf("subgraph node has no compiled program")
	}
	childRT := runtime.New(sub, rt.Graph(),
		runtime.WithSeedScope(rt.Scope()),
		runtime.WithMaxCollectionSize(rt.MaxCollectionSize()),
	)

	prefix := n.ID() + "/"
	// The child executor carries the qualified frame prefix so every identity
	// sink it writes — the coordinator watch key, the kro.run/node-id
	// label/selector token, and the node-path annotation — is qualified at the
	// point of construction and stays mutually consistent. (Previously a
	// prefixWatcher rewrote only the watch key, leaving the label and selector
	// unqualified, so sibling subgraphs reusing a local node ID cross-matched
	// each other's items.) The store NodeID and unresolved IDs are still
	// prefixed below, because managedResourceFrom records the raw local ID.
	child := *s
	child.nodePrefix = s.nodePrefix + prefix
	childResult, applyErr := child.Apply(ctx, childRT, w)
	applied := make([]expv1alpha1.ManagedResource, 0, len(childResult.Applied))
	for _, mr := range childResult.Applied {
		mr.NodeID = prefix + mr.NodeID
		applied = append(applied, mr)
	}
	unresolved := make([]string, 0, len(childResult.Unresolved))
	for _, u := range childResult.Unresolved {
		unresolved = append(unresolved, prefix+u)
	}
	contribs := childResult.Contributions

	// Publish the child's node outputs as a map under the subgraph node ID,
	// best-effort even on a soft error so the children that did resolve are
	// visible to downstream parent expressions.
	out := make(map[string]any, len(childRT.Nodes()))
	for _, childNode := range childRT.Nodes() {
		if v, ok := childRT.Scope()[childNode.ID()]; ok {
			out[childNode.ID()] = v
		}
	}
	rt.Set(n.ID(), out)

	return applied, unresolved, contribs, applyErr
}

// mappingFor returns the GVR and REST scope to apply obj under. Static
// nodes use the GVR resolved at compile time. Dynamic-GVK nodes resolve
// the rendered object's concrete GVK through the live REST mapper; a
// missing mapping (CRD not installed yet) becomes errSchemaNotReady.
func (s *Simple) mappingFor(n *runtime.Node, obj *unstructured.Unstructured) (schema.GroupVersionResource, bool, error) {
	if !n.DynamicGVK() {
		return n.GVR(), n.Namespaced(), nil
	}
	gvk := obj.GroupVersionKind()
	if gvk.Kind == "" || gvk.Version == "" {
		return schema.GroupVersionResource{}, false, fmt.Errorf("dynamic template resolved to incomplete GVK %q", gvk.String())
	}
	m, err := s.Client.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return schema.GroupVersionResource{}, false, fmt.Errorf("%s: %w", gvk.String(), errSchemaNotReady)
		}
		return schema.GroupVersionResource{}, false, fmt.Errorf("rest mapping for %s: %w", gvk.String(), err)
	}
	return m.Resource, m.Scope.Name() == meta.RESTScopeNameNamespace, nil
}

// watchObject registers a scalar watch on a resolved Template object so the
// dynamic controller re-enqueues the Graph when the resource changes out
// from under us. Cluster-scoped resources are watched with Namespace="".
// A nil watcher is treated as a Noop.
//
// When CanWatch is set (impersonated Graph path) it gates the registration on
// whether the applying identity may watch the target GVR: a denied or
// inconclusive check skips the watch (drift detection degraded for that GVR)
// and returns nil so the caller still applies the object.
func (s *Simple) watchObject(ctx context.Context, w watchrouter.Watcher, nodeID string, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) error {
	if w == nil {
		return nil
	}
	if s.skipWatchForIdentity(ctx, gvr, obj.GetNamespace()) {
		return nil
	}
	return w.Watch(watchrouter.WatchRequest{
		NodeID:    nodeID,
		GVR:       gvr,
		Name:      obj.GetName(),
		Namespace: obj.GetNamespace(),
	})
}

// skipWatchForIdentity reports whether the drift watch on gvr/namespace should
// be skipped because CanWatch denies (or cannot confirm) that the applying
// identity may watch it. Nil CanWatch (RGD/instance path) never skips. A denied
// or inconclusive check logs that drift detection is degraded and returns true.
func (s *Simple) skipWatchForIdentity(ctx context.Context, gvr schema.GroupVersionResource, namespace string) bool {
	if s.CanWatch == nil {
		return false
	}
	allowed, err := s.CanWatch(ctx, gvr, namespace)
	if err != nil {
		log.FromContext(ctx).Info("skipping drift watch: access review inconclusive; drift detection degraded for this GVR",
			"gvr", gvr.String(), "namespace", namespace, "err", err.Error())
		return true
	}
	if !allowed {
		log.FromContext(ctx).Info("skipping drift watch: impersonated ServiceAccount may not watch this GVR; drift detection degraded",
			"gvr", gvr.String(), "namespace", namespace)
		return true
	}
	return false
}

// collectionWatchNamespace returns the namespace to scope a collection node's
// selector watch to. A collection can template items into DIFFERENT namespaces
// (each item's metadata.namespace is CEL-derived), so the watch must span all
// of them in that case. But the common case is a single namespace, and a
// namespaced watch is cheaper than an all-namespaces one — so scope the watch
// when every resolved item lands in the same namespace, and fall back to ""
// (all-namespaces) only when they genuinely differ.
//
// desired is the fully-resolved item set (namespaces already CEL-evaluated),
// so this decision is exact. Items are not yet namespace-defaulted here
// (prepareItem does that inside the apply goroutines), so the same defaulting
// is applied read-only: an empty namespace on a namespaced kind resolves to the
// Graph namespace. A genuinely-empty namespace (cluster-scoped, or a namespaced
// item that would fail validation) yields "" so the watch stays broad rather
// than wrong.
func (s *Simple) collectionWatchNamespace(rt *runtime.Runtime, desired []*unstructured.Unstructured, mappings []applyMapping) string {
	graphNS := rt.Graph().GetNamespace()
	var seen string
	for i, obj := range desired {
		ns := obj.GetNamespace()
		if ns == "" && mappings[i].namespaced {
			ns = graphNS
		}
		if ns == "" {
			// Cluster-scoped, or an unresolved namespace we can't scope to:
			// keep the watch all-namespaces.
			return ""
		}
		if seen == "" {
			seen = ns
			continue
		}
		if ns != seen {
			// The collection spans multiple namespaces — must watch all.
			return ""
		}
	}
	return seen
}

// watchMapping pairs a distinct collection GVR with a representative rendered
// object of that GVR, used to register one selector drift watch per GVR.
type watchMapping struct {
	gvr    schema.GroupVersionResource
	sample *unstructured.Unstructured
}

// distinctWatchMappings returns one (gvr, sample) pair per DISTINCT GVR across a
// collection's items, preserving first-seen order. A static collection collapses
// to a single entry; a dynamic-GVK collection that renders several GVRs yields
// one entry each so every rendered type gets a drift watch. desired and mappings
// are index-aligned (buildMappings guarantees len(mappings)==len(desired)).
func distinctWatchMappings(desired []*unstructured.Unstructured, mappings []applyMapping) []watchMapping {
	out := make([]watchMapping, 0, 1)
	seen := make(map[schema.GroupVersionResource]struct{}, 1)
	for i := range desired {
		gvr := mappings[i].gvr
		if _, dup := seen[gvr]; dup {
			continue
		}
		seen[gvr] = struct{}{}
		out = append(out, watchMapping{gvr: gvr, sample: desired[i]})
	}
	return out
}

// watchCollection registers a single selector-based watch for a collection
// node. The coordinator keys watch state by NodeID, so N per-item scalar
// watches would collapse to only the last item and drift on the others would
// go unobserved. One selector watch matching {instance-id, node-id-token}
// tracks every item under this node. The node-id token is the bounded,
// label-safe rendering of the node's qualified path (nodeIDToken), matching
// the value stampKROMeta writes into the kro.run/node-id label — so two sibling
// subgraphs that reuse the same local node ID get distinct tokens
// ("subA.res" vs "subB.res") and no longer cross-match each other's items. The
// instance-id label scopes the watch to THIS Graph: the RGD/instance path
// stamps it via LabelInjector; the standalone Graph path (no LabelInjector)
// falls back to the Graph's own UID. Without it, two Graphs whose collection
// nodes share a node id would register byte-identical watches and wake each
// other on every event.
//
// ns is the watch namespace computed by collectionWatchNamespace: a single
// namespace when the whole collection lands there, or "" (all-namespaces) when
// it spans namespaces or is cluster-scoped. Scoping to a single namespace is a
// cheaper watch than the all-namespaces one; the broad watch is used only when
// the collection genuinely needs it (see the "corrects drift ... in every
// namespace the collection spans" integration test).
func (s *Simple) watchCollection(ctx context.Context, w watchrouter.Watcher, rt *runtime.Runtime, n *runtime.Node, gvr schema.GroupVersionResource, sample *unstructured.Unstructured, ns string) error {
	if w == nil {
		return nil
	}
	if s.skipWatchForIdentity(ctx, gvr, ns) {
		return nil
	}
	if s.LabelInjector != nil {
		s.LabelInjector(sample)
	}
	set := labels.Set{metadata.NodeIDLabel: s.nodeIDToken(n.ID())}
	// Scope to this Graph: fall back to the Graph UID when no LabelInjector
	// stamped an instance-id (standalone Graph path).
	uid := sample.GetLabels()[metadata.InstanceIDLabel]
	if uid == "" {
		uid = string(rt.Graph().GetUID())
	}
	if uid != "" {
		set[metadata.InstanceIDLabel] = uid
	}
	return w.Watch(watchrouter.WatchRequest{
		NodeID:    s.qualifiedPath(n.ID()),
		GVR:       gvr,
		Namespace: ns,
		Selector:  labels.SelectorFromSet(set),
	})
}

// defaultNamespace fills in metadata.namespace from the Graph for
// namespaced resources when the template left it empty. Cluster-scoped
// resources are untouched.
func (s *Simple) defaultNamespace(rt *runtime.Runtime, namespaced bool, obj *unstructured.Unstructured) {
	if !namespaced || obj.GetNamespace() != "" {
		return
	}
	if ns := rt.Graph().GetNamespace(); ns != "" {
		obj.SetNamespace(ns)
	}
}

// ssaApply server-side applies obj with the given field manager. force takes
// ownership of fields already owned by other managers so a template re-apply
// after a hand-edit converges back; a patch contributes without force so it
// only claims the fields it sets.
func (s *Simple) ssaApply(ctx context.Context, obj *unstructured.Unstructured, fieldManager string, force bool) error {
	if s.LabelInjector != nil {
		s.LabelInjector(obj)
	}
	opts := []client.PatchOption{client.FieldOwner(fieldManager)}
	if force {
		opts = append(opts, client.ForceOwnership)
	}
	return s.Client.Patch(ctx, obj, client.Apply, opts...)
}

// applyPatch contributes the fields a patch node produced to a target it does
// not own. The target must already exist: an absent target is a soft requeue
// (ErrNotReady) so dependents gate and the reconcile retries once the target
// appears. The contribution is server-side applied under a per-node field
// manager without ForceOwnership, so it claims only the fields it sets; a
// status-subresource patch is routed through the status endpoint. Returns the
// recorded Contribution so the reconciler can release it on prune.
func (s *Simple) applyPatch(ctx context.Context, rt *runtime.Runtime, w watchrouter.Watcher, n *runtime.Node, desired []*unstructured.Unstructured) ([]Contribution, error) {
	// A patch node may be a singleton (one target) or a forEach collection (the
	// same contribution fanned out across every rendered target, e.g. a status
	// writeback to each claimant CR). An empty desired set (forEach over an empty
	// list) is a no-op, not an error.
	//
	// Every target is patched BEST-EFFORT: a failure on one target (e.g. it is
	// not present yet) must not prevent patching the targets that DO resolve, so
	// we visit all of them and combine the errors afterwards rather than aborting
	// on the first. Errors are partitioned so a HARD error still dominates: a
	// malformed or permanently-rejected target is not masked by a sibling's soft
	// not-ready. When only soft errors occurred the node stays soft not-ready, so
	// the reconcile requeues and retries the not-yet-present targets while the
	// resolved ones are already patched.
	collection := n.IsCollection()
	contributions := make([]Contribution, 0, len(desired))
	var softErrs, hardErrs []error
	for _, obj := range desired {
		contribution, err := s.applyPatchOne(ctx, rt, w, n, obj, collection)
		if err != nil {
			if errors.Is(err, errSchemaNotReady) || errors.Is(err, ErrNotReady) || isSoftRuntimeErr(err) {
				softErrs = append(softErrs, err)
			} else {
				hardErrs = append(hardErrs, err)
			}
			continue
		}
		contributions = append(contributions, contribution)
	}
	if len(hardErrs) > 0 {
		return contributions, errors.Join(hardErrs...)
	}
	if len(softErrs) > 0 {
		return contributions, errors.Join(softErrs...)
	}
	return contributions, nil
}

// applyPatchOne contributes a single rendered patch object to its target. Each
// target gets a drift watch so a manual edit or deletion of a patched field
// re-enqueues the Graph and the contribution is re-applied. For a forEach
// (collection) patch the watch is registered under a per-TARGET synthetic node
// ID ("<node>#<ns>/<name>") rather than the bare node ID: every fanned-out
// target shares one node ID and one GVR, so a single shared key would collapse
// them in the coordinator's (nodeID,GVR) watch state and only the last target
// would keep a live watch. The synthetic ID gives each target its own entry;
// when the forEach set shrinks, the dropped target's watch is torn down on the
// next Done(true) because its synthetic ID is no longer re-declared. The one
// target NEVER watched is a self-watch-exempt node (the synthesized author-
// status writeback): its target is the reconciled instance's own status
// subresource, so watching it would re-enqueue the instance on its own status
// write — a self-perpetuating loop, since the drift-watch enqueue path is not
// generation-guarded and a status write doesn't bump generation. The parent
// informer already drives that instance's reconciliation.
func (s *Simple) applyPatchOne(ctx context.Context, rt *runtime.Runtime, w watchrouter.Watcher, n *runtime.Node, obj *unstructured.Unstructured, collection bool) (Contribution, error) {
	gvr, namespaced, err := s.mappingFor(n, obj)
	if err != nil {
		return Contribution{}, err
	}
	s.defaultNamespace(rt, namespaced, obj)

	// Register the watch before the read so a change to the target (including
	// its creation) re-enqueues the Graph. Skipped only for a self-watch-exempt
	// node (see the doc comment): the instance's own status writeback must not
	// re-enqueue itself. A forEach target is watched under a per-target synthetic
	// node ID so N targets sharing one node/GVR each keep a live drift watch.
	if !n.SelfWatchExempt() {
		watchNodeID := s.qualifiedPath(n.ID())
		if collection {
			watchNodeID = fmt.Sprintf("%s#%s/%s", watchNodeID, obj.GetNamespace(), obj.GetName())
		}
		if err := s.watchObject(ctx, w, watchNodeID, gvr, obj); err != nil {
			// A failure to register the drift watch must not abort the apply: the
			// patch contribution below still lands, only drift re-enqueue is lost.
			// Log and continue.
			log.FromContext(ctx).Info("drift watch registration failed; applying patch without drift detection",
				"node", watchNodeID, "gvr", gvr.String(), "err", err.Error())
		}
	}

	current, err := s.getLive(ctx, obj)
	if err != nil {
		return Contribution{}, err
	}
	if current == nil {
		return Contribution{}, fmt.Errorf("patch target %s %q not found: %w",
			obj.GetKind(), client.ObjectKeyFromObject(obj), ErrNotReady)
	}
	if current.GetDeletionTimestamp() != nil {
		return Contribution{}, &ResourceDeletingError{NodeID: s.qualifiedPath(n.ID()), Namespace: obj.GetNamespace(), Name: obj.GetName()}
	}

	fieldManager := patchFieldManager(rt.Graph().GetUID(), s.qualifiedPath(n.ID()))
	subresource := n.Subresource()
	if err := s.contributeApply(ctx, obj, fieldManager, subresource); err != nil {
		if apierrors.IsConflict(err) {
			return Contribution{}, fmt.Errorf("patch field conflict on %s %q: %w (%w)",
				obj.GetKind(), client.ObjectKeyFromObject(obj), err, ErrNotReady)
		}
		return Contribution{}, err
	}

	gvk := obj.GroupVersionKind()
	return Contribution{
		APIVersion:   gvk.GroupVersion().String(),
		Kind:         gvk.Kind,
		Namespace:    obj.GetNamespace(),
		Name:         obj.GetName(),
		Subresource:  subresource,
		FieldManager: fieldManager,
	}, nil
}

// contributeApply server-side applies obj under fieldManager. A status patch
// forces ownership so status writeback reclaims fields from a legacy Update
// manager. A non-status patch applies unforced so the API server reports a
// field-level 409 rather than silently stealing a field owned by a human,
// another controller, or a peer Graph — the caller surfaces that as soft
// not-ready. The exception is a conflict where EVERY conflicting field is owned
// by this Graph's OWN stale patch identity: unforced it would deadlock forever,
// so we re-apply with force, which SSA scopes to only the fields this manager
// sets.
//
// The force decision is driven by the 409's own conflict causes — the exact
// set of (field, owning-manager) pairs the apiserver refused — not by scanning
// the whole live object. Two consequences the reviewer required (finding
// 3901227029): (1) ownership of fields that did NOT conflict is irrelevant, so
// owning some unrelated field on the target can no longer make us force-steal a
// foreign owner's conflicting field; and (2) the causes are evaluated as-of the
// failed apply, so a peer that added a field between an earlier read and now
// cannot be force-overwritten on the strength of a stale snapshot. If ANY
// conflicting field is owned by a foreign/peer manager (or the causes can't be
// parsed), we do not force and surface the conflict as soft not-ready.
func (s *Simple) contributeApply(ctx context.Context, obj *unstructured.Unstructured, fieldManager, subresource string) error {
	if subresource == "status" {
		return s.Client.Status().Patch(ctx, obj, client.Apply, client.FieldOwner(fieldManager), client.ForceOwnership)
	}
	err := s.ssaApply(ctx, obj, fieldManager, false)
	if err == nil || !apierrors.IsConflict(err) {
		return err
	}
	// Force-reclaim only when every conflicting field is our own stale identity;
	// leave a foreign/peer conflict for the caller to surface as soft not-ready.
	if allConflictsAreReclaimableStalePatchManagers(err, fieldManager) {
		return s.ssaApply(ctx, obj, fieldManager, true)
	}
	return err
}

// Release relinquishes the fields each contribution's field manager owns by
// server-side applying an identity-only object under that manager. SSA drops
// every field the manager previously owned; the target object and other
// managers' fields are left intact. A missing target is tolerated.
func (s *Simple) Release(ctx context.Context, contributions []Contribution) error {
	for _, c := range contributions {
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion(c.APIVersion)
		obj.SetKind(c.Kind)
		obj.SetNamespace(c.Namespace)
		obj.SetName(c.Name)
		name := refName(expv1alpha1.ManagedResource{Namespace: c.Namespace, Name: c.Name})

		// Release relinquishes this manager's field-ownership claim on an
		// EXISTING target. GET first so we never recreate a target that was
		// legitimately deleted: a server-side Apply of an identity-only object
		// with no live object present would CREATE it (a bare, unwanted
		// resource). Only patch when the object is confirmed present.
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(obj.GroupVersionKind())
		getErr := s.Client.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: c.Name}, live)
		if getErr != nil {
			// Target object gone (NotFound) or its type gone (the CRD was
			// removed → NoMatch): nothing to release, treat as already-released.
			if apierrors.IsNotFound(getErr) || meta.IsNoMatchError(getErr) {
				continue
			}
			// A transient discovery/transport failure (apiserver unavailable,
			// throttling) must NOT be mistaken for a missing target — return it
			// so the caller retries rather than silently dropping the release.
			return fmt.Errorf("release %s/%s %s: get target: %w",
				c.APIVersion, c.Kind, name, getErr)
		}

		var err error
		if c.Subresource == "status" {
			err = s.Client.Status().Patch(ctx, obj, client.Apply, client.FieldOwner(c.FieldManager))
		} else {
			err = s.Client.Patch(ctx, obj, client.Apply, client.FieldOwner(c.FieldManager))
		}
		if err != nil {
			// The target was deleted between our GET and the patch: still
			// already-released, tolerate. (A NoMatch here would likewise mean
			// the type went away in the same window.)
			if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				continue
			}
			return fmt.Errorf("release %s/%s %s (manager %q): %w",
				c.APIVersion, c.Kind, name, c.FieldManager, err)
		}
	}
	return nil
}

// patchFieldManagerPrefix marks a field manager as a kro Graph patch writer.
const patchFieldManagerPrefix = "kro-graphengine.patch."

// patchFieldManager derives a stable per-Graph patch identity formatted as
// "<prefix><graphSeg>.<nodeSeg>": graphSeg is sha256(parentUID)[:12], nodeSeg
// is sha256(parentUID+"/"+nodeID)[:12]. Qualifying by nodeID keeps sibling
// subgraphs that reuse a local id distinct; embedding graphSeg lets the
// conflict classifier recognize two managers of the same Graph as our own
// (vs a peer). nodeID is the node's fully-qualified path (e.g. "subA/res").
//
// Exported as PatchFieldManager so the graph controller's contribution
// write-ahead projects the SAME field-manager identity the executor will apply
// under — the two must not drift, or a write-ahead ledger entry would fail to
// correlate with the contribution Release later looks for.
func patchFieldManager(parentUID types.UID, nodeID string) string {
	return PatchFieldManager(parentUID, nodeID)
}

// PatchFieldManager is the exported, drift-proof derivation of a patch node's
// field-manager identity. See patchFieldManager. parentUID is the Graph UID;
// nodeID is the node's fully-qualified path.
func PatchFieldManager(parentUID types.UID, nodeID string) string {
	return fieldManager(patchFieldManagerPrefix, parentUID, nodeID)
}

// fieldManager derives a stable "<prefix><graphSegment>.<nodeSegment>"
// field-manager identity shared by the patch and template derivations.
func fieldManager(prefix string, parentUID types.UID, nodeID string) string {
	h := sha256.New()
	h.Write([]byte(parentUID))
	h.Write([]byte("/"))
	h.Write([]byte(nodeID))
	sum := h.Sum(nil)
	return prefix + graphManagerSegment(parentUID) + "." + hex.EncodeToString(sum[:6])
}

// IsPatchFieldManager reports whether manager is one of kro's dedicated patch
// server-side-apply field managers (prefix "kro-graphengine.patch."). The
// finalizer's Release path uses this to refuse relinquishing a NON-kro manager:
// on the instance path the patch-contribution inventory is a client-editable
// annotation (the instance is a dynamic RGD-generated CR whose status cannot
// carry an internal kro field), so a principal with patch rights on the
// instance could forge a contribution naming an arbitrary field manager (e.g.
// "kubectl" or another controller's) on some target and weaponize kro's
// privileged finalizer to strip that manager's fields off the object. Only a
// kro patch manager is ever legitimately recorded, so releasing anything else
// is refused. (Release is already non-destructive — it relinquishes field
// ownership, never deletes — and GET-first; this closes the cross-manager
// escalation the annotation would otherwise allow.)
func IsPatchFieldManager(manager string) bool {
	return strings.HasPrefix(manager, patchFieldManagerPrefix)
}

// patchManagerGraphSegment returns the per-Graph segment of a patch manager
// ("<prefix><graphSeg>.<nodeSeg>"), or "" for a non-patch manager or a legacy
// pre-segment manager ("<prefix><hash>", no dot).
func patchManagerGraphSegment(manager string) string {
	return managerGraphSegment(patchFieldManagerPrefix, manager)
}

// managerGraphSegment extracts the per-Graph segment from a field manager
// formatted as "<prefix><graphSegment>.<nodeSegment>". Returns "" for a manager
// without the given prefix or without a dot (a legacy pre-segment manager).
func managerGraphSegment(prefix, manager string) string {
	rest, ok := strings.CutPrefix(manager, prefix)
	if !ok {
		return ""
	}
	if seg, _, found := strings.Cut(rest, "."); found {
		return seg
	}
	return ""
}

// allConflictsAreReclaimableStalePatchManagers reports whether a 409 apply
// conflict is composed ENTIRELY of fields owned by this Graph's own stale patch
// identity — the only case where force-reclaiming is safe. It reads the
// managers named in the error's conflict causes (the apiserver's exact list of
// contested (field, owner) pairs), so the decision is scoped to the fields that
// actually conflicted and reflects state as-of the failed apply. If any
// conflicting field is owned by a foreign/peer manager, or the error carries no
// parseable field-manager conflict causes, it returns false so the caller does
// NOT force and instead surfaces the conflict as soft not-ready.
func allConflictsAreReclaimableStalePatchManagers(err error, self string) bool {
	status := apierrors.APIStatus(nil)
	if !errors.As(err, &status) || status.Status().Details == nil {
		return false
	}
	selfGraph := patchManagerGraphSegment(self)
	sawConflict := false
	for _, cause := range status.Status().Details.Causes {
		if cause.Type != metav1.CauseTypeFieldManagerConflict {
			continue
		}
		sawConflict = true
		manager := conflictCauseManager(cause.Message)
		if !isReclaimableStalePatchManager(manager, self, selfGraph) {
			return false
		}
	}
	return sawConflict
}

// isReclaimableStalePatchManager reports whether a single conflicting manager is
// this Graph's own stale patch identity — safe to force-reclaim. That is a
// manager sharing self's per-Graph segment, or a legacy pre-segment patch
// manager (only an older kro build produced those). self itself, a peer Graph's
// patch manager (different segment), and any non-kro manager are not
// reclaimable. An unparseable/empty manager is not reclaimable.
func isReclaimableStalePatchManager(manager, self, selfGraph string) bool {
	if manager == "" || manager == self || !strings.HasPrefix(manager, patchFieldManagerPrefix) {
		return false
	}
	seg := patchManagerGraphSegment(manager)
	return seg == "" || (selfGraph != "" && seg == selfGraph)
}

// conflictCauseManager extracts the field-manager name from an SSA conflict
// cause message. The apiserver formats these as `conflict with "<manager>"`
// optionally followed by ` using <version>[ at <time>]` or ` with subresource
// "<sub>"` (see apimachinery managedfields printManager), so the manager name
// is the first double-quoted token. Returns "" when no quoted token is present.
func conflictCauseManager(message string) string {
	start := strings.IndexByte(message, '"')
	if start < 0 {
		return ""
	}
	end := strings.IndexByte(message[start+1:], '"')
	if end < 0 {
		return ""
	}
	return message[start+1 : start+1+end]
}

// templateFieldManagerPrefix marks a field manager as a kro Graph template
// writer. The classifier keys on this prefix to tell a peer Graph's ownership
// (reject) apart from external drift (force-reclaim).
const templateFieldManagerPrefix = "kro-graphengine.tmpl."

// graphManagerSegment derives the per-Graph segment embedded in a template
// field manager: the first 12 hex characters of sha256(parentUID). Two nodes
// in the SAME Graph share this segment even though their full managers differ
// by nodeID, so the conflict classifier can tell a self-conflict (same Graph,
// different node) apart from a peer-Graph conflict (different Graph).
func graphManagerSegment(parentUID types.UID) string {
	h := sha256.New()
	h.Write([]byte(parentUID))
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:6])
}

// ErrFieldManagerConflict is the sentinel returned when a Template object's
// field is already owned by a DIFFERENT kro Graph's template field manager.
// kro refuses to force-steal a peer Graph's field, so the node is held soft
// not-ready and the reconcile backs off instead of flip-flopping the field
// between the two Graphs forever. It always also satisfies
// errors.Is(err, ErrNotReady) at the call site (wrapped alongside it).
var ErrFieldManagerConflict = errors.New("executor: field owned by a foreign field manager")

// templateFieldManager derives a stable, PER-GRAPH field-manager identity for a
// Template node, formatted as "<prefix><graphSegment>" where graphSegment is
// the first 12 hex characters of sha256(parentUID). It is keyed on the Graph
// UID ONLY — deliberately NOT on the nodeID.
//
// Keying per-Graph (not per-node) keeps object ownership STABLE across a node
// rename: when a node "old" that applied {data.a, data.b} is renamed to "new"
// applying only {data.a}, both write under the same manager, so SSA narrows the
// manager's field set and drops data.b instead of orphaning it under a retired
// per-node manager (which nothing applies under anymore, so SSA never reaps it).
// Two nodes of one Graph can never legitimately co-own one object — the
// identity-claim guard rejects that before any write — so a per-node manager was
// never needed to keep them apart, and dropping it also removes the same-Graph
// self-conflict case entirely. Two DISTINCT Graphs still get distinct managers
// (distinct graphSegment), so SSA reports a field-level conflict rather than
// silently reassigning ownership between Graphs.
func templateFieldManager(parentUID types.UID) string {
	return templateFieldManagerPrefix + graphManagerSegment(parentUID)
}

// ownedByForeignGraphTemplate reports whether current carries a managedFields
// entry under a kro Graph template manager (templateFieldManagerPrefix) that
// belongs to a DIFFERENT Graph than self. It is how a template apply tells a
// peer Graph's ownership (which must not be stolen) apart from external drift
// by a human or another controller (which a forced re-apply legitimately
// reclaims). Because the template manager is now per-Graph, self's own prior
// apply matches `mf.Manager == self` directly; the per-Graph segment comparison
// additionally exempts a legacy per-node manager of the SAME Graph that may
// still be present on an object during a controller rolling upgrade. A nil
// current (absent object) is never foreign-owned.
func ownedByForeignGraphTemplate(current *unstructured.Unstructured, self string) bool {
	if current == nil {
		return false
	}
	selfGraph := templateManagerGraphSegment(self)
	for _, mf := range current.GetManagedFields() {
		if mf.Manager == self {
			continue
		}
		if !strings.HasPrefix(mf.Manager, templateFieldManagerPrefix) {
			continue
		}
		// Same Graph (shared per-Graph segment): our own ownership, e.g. a legacy
		// per-node manager left on the object by a pre-upgrade controller. Exempt
		// it so a Graph never reports itself as a foreign peer.
		if selfGraph != "" && templateManagerGraphSegment(mf.Manager) == selfGraph {
			continue
		}
		return true
	}
	return false
}

// ownedByGraphTemplate reports whether current carries ANY kro Graph template
// manager (templateFieldManagerPrefix). It is the RGD/instance path's
// cross-engine ownership guard: on that path (ConflictDetection off) the
// executor NEVER writes a template manager, so any such manager on the live
// object is definitionally a foreign Graph's field. The RGD path must refuse
// it rather than force-adopting it under the shared field manager, mirroring
// the Graph path's peer-conflict refusal (the two engines' detection would
// otherwise be asymmetric — the Graph never stamps the applyset part-of label
// the RGD guard keys on, so the RGD path would silently steal a Graph's field).
// A nil current (absent object) is never Graph-owned.
func ownedByGraphTemplate(current *unstructured.Unstructured) bool {
	if current == nil {
		return false
	}
	for _, mf := range current.GetManagedFields() {
		if strings.HasPrefix(mf.Manager, templateFieldManagerPrefix) {
			return true
		}
	}
	return false
}

// templateManagerGraphSegment extracts the per-Graph segment from a template
// field manager. The current per-Graph manager is "<prefix><graphSegment>" (no
// dot); a legacy per-node manager was "<prefix><graphSegment>.<nodeSegment>".
// Returns the segment before the dot when present, otherwise the whole rest
// after the prefix, so both shapes resolve to the same graphSegment during a
// rolling upgrade. Returns "" for a manager without the tmpl prefix.
func templateManagerGraphSegment(manager string) string {
	rest, ok := strings.CutPrefix(manager, templateFieldManagerPrefix)
	if !ok {
		return ""
	}
	if seg, _, found := strings.Cut(rest, "."); found {
		return seg
	}
	return rest
}

// anyConflictOwnedByForeignGraphTemplate reports whether a 409 apply conflict
// names at least one field owned by a PEER Graph's template manager — the case
// where force-reclaiming would steal a peer's field and must be refused. Like
// the patch path's classifier it reads the managers named in the error's own
// conflict causes (the apiserver's exact list of contested (field, owner)
// pairs) rather than a pre-apply snapshot of the live object, so the decision
// reflects state AS-OF the failed apply: a peer that took ownership between an
// earlier read and this apply is the one that actually caused the 409 and is
// seen here, closing the read-then-write race the snapshot check had (a stale
// `current` that predates the peer's write would wrongly report no foreign
// owner and force-steal the field).
//
// External drift (kubectl, another controller — a non-tmpl manager) is NOT
// foreign-Graph-owned, so it returns false and the caller reclaims with force,
// preserving drift correction. A conflict with this Graph's OWN manager (same
// per-Graph segment, e.g. a legacy per-node manager mid-upgrade) is likewise
// not foreign. To stay conservative it treats an unparseable error, or a
// conflict cause whose manager can't be read, as foreign (returns true) so a
// contested field is never force-stolen on ambiguous evidence.
func anyConflictOwnedByForeignGraphTemplate(err error, self string) bool {
	status := apierrors.APIStatus(nil)
	if !errors.As(err, &status) || status.Status().Details == nil {
		return true
	}
	selfGraph := templateManagerGraphSegment(self)
	for _, cause := range status.Status().Details.Causes {
		if cause.Type != metav1.CauseTypeFieldManagerConflict {
			continue
		}
		if isForeignGraphTemplateManager(conflictCauseManager(cause.Message), self, selfGraph) {
			return true
		}
	}
	return false
}

// isForeignGraphTemplateManager reports whether a single conflicting manager is
// a PEER Graph's template writer. self itself and a same-Graph manager (shared
// per-Graph segment, incl. a legacy per-node manager) are our own, not foreign.
// A non-tmpl manager is external drift, not foreign-Graph-owned. An
// unparseable/empty manager is treated as foreign so an ambiguous conflict is
// never force-reclaimed.
func isForeignGraphTemplateManager(manager, self, selfGraph string) bool {
	if manager == "" {
		return true
	}
	if manager == self || !strings.HasPrefix(manager, templateFieldManagerPrefix) {
		return false
	}
	return selfGraph == "" || templateManagerGraphSegment(manager) != selfGraph
}

// applyTemplateObject SSA-applies a resolved Template object, choosing the
// ownership strategy from ConflictDetection.
//
// Off (RGD/instance path, unchanged): a single forced apply under the shared
// FieldManager. Drift — including a hand-edit — converges back on the next
// reconcile, and cross-owner contention is caught earlier by the ApplySet
// part-of guard.
//
// On (standalone Graph path): apply under a per-Graph template manager WITHOUT
// force so the API server reports a field-level 409 when a field is already
// owned by another manager. If that other manager is a peer Graph's template
// writer, return ErrFieldManagerConflict (soft not-ready) — kro will not steal
// a peer's field, which is what stops two Graphs from flip-flopping the same
// object. If the conflicting owner is instead external drift (kubectl, another
// controller), re-apply WITH force to reclaim it, preserving drift correction.
//
// The peer-vs-drift decision reads the 409's OWN conflict causes
// (anyConflictOwnedByForeignGraphTemplate), not a pre-apply snapshot of the
// live object: a snapshot read before the apply can miss a peer that took
// ownership in the read-then-write window and then get force-stolen on stale
// evidence (reviewer finding 3909839245). Basing the decision on the causes of
// the apply that actually failed evaluates ownership as-of that apply.
func (s *Simple) applyTemplateObject(ctx context.Context, obj *unstructured.Unstructured, parentUID types.UID) error {
	if !s.ConflictDetection {
		return s.ssaApply(ctx, obj, FieldManager, true)
	}

	fieldManager := templateFieldManager(parentUID)
	err := s.ssaApply(ctx, obj, fieldManager, false)
	if err == nil || !apierrors.IsConflict(err) {
		return err
	}
	// A field is owned by another manager. Reject only if a conflicting field is
	// owned by a peer Graph's template writer; otherwise the conflict is external
	// drift we are allowed to reclaim with force.
	if anyConflictOwnedByForeignGraphTemplate(err, fieldManager) {
		return fmt.Errorf("template %s %q: %w", obj.GetKind(), client.ObjectKeyFromObject(obj), ErrFieldManagerConflict)
	}
	return s.ssaApply(ctx, obj, fieldManager, true)
}
