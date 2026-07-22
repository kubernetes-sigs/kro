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
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

// Simple is the v1 executor: walk nodes in topological order, SSA-apply
// each Template, record observed state on the runtime so dependents see
// the live cluster values, and on Delete tear them down in reverse.
//
// Ignored nodes (includeWhen=false, or contagiously via an ignored
// upstream) are skipped entirely — no resolve, no apply, no scope
// publication. ReadyWhen checks gate the loop: an unsatisfied readyWhen
// returns ErrNotReady so the reconciler requeues.
type Simple struct {
	Client client.Client
}

// NewSimple constructs a Simple executor bound to the given client.
func NewSimple(c client.Client) *Simple {
	return &Simple{Client: c}
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
func (s *Simple) Apply(ctx context.Context, rt *runtime.Runtime, w watchrouter.Watcher) (ApplyResult, error) {
	var result ApplyResult
	var firstSoft error
	recordSoft := func(err error) {
		if firstSoft == nil {
			firstSoft = err
		}
	}

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
			return result, fmt.Errorf("apply %q: %w", n.ID(), err)
		}
		if ignored {
			// Intentionally skipped — not Unresolved. The caller
			// will treat any previous entries for this NodeID as
			// prune candidates.
			continue
		}

		// A subgraph node has no payload of its own — it runs a child
		// Program in a scope seeded from this one. Handle it before
		// Resolve (which would deref the nil Object).
		if n.Kind() == compiler.NodeKindGraph {
			applied, unresolved, err := s.applySubgraph(ctx, rt, w, n)
			result.Applied = append(result.Applied, applied...)
			result.Unresolved = append(result.Unresolved, unresolved...)
			if err != nil {
				if errors.Is(err, ErrNotReady) || isSoftRuntimeErr(err) {
					recordSoft(fmt.Errorf("apply %q (subgraph): %w", n.ID(), err))
					continue
				}
				return result, fmt.Errorf("apply %q (subgraph): %w", n.ID(), err)
			}
			continue
		}

		desired, err := n.Resolve()
		if err != nil {
			if isSoftRuntimeErr(err) {
				result.Unresolved = append(result.Unresolved, n.ID())
				recordSoft(fmt.Errorf("apply %q: resolve: %w (%w)", n.ID(), err, ErrNotReady))
				continue
			}
			return result, fmt.Errorf("apply %q: resolve: %w", n.ID(), err)
		}
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
					recordSoft(fmt.Errorf("apply %q: %w (%w)", n.ID(), err, ErrNotReady))
					continue
				}
				return result, fmt.Errorf("apply %q: %w", n.ID(), err)
			}
			n.SetObserved(desired, desired)
			publishScope(rt, n, n.Observed())
		case compiler.NodeKindRef, compiler.NodeKindWatch:
			return result, fmt.Errorf("apply %q (%s): %w", n.ID(), n.Kind(), ErrUnsupported)
		default:
			return result, fmt.Errorf("apply %q: unknown kind %v", n.ID(), n.Kind())
		}

		// readyWhen is checked after observed state is recorded.
		// Soft → ErrNotReady (already tracked in Applied); continue
		// so downstream watches still register. Hard → abort.
		if err := n.CheckReadiness(); err != nil {
			if isSoftRuntimeErr(err) {
				recordSoft(fmt.Errorf("apply %q: %w (%w)", n.ID(), err, ErrNotReady))
				continue
			}
			return result, fmt.Errorf("apply %q: %w", n.ID(), err)
		}
	}
	return result, firstSoft
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
// UID precondition guards against deleting an impostor that some other
// actor created after the resource we tracked was removed out of band.
// NotFound and "already deleted by something else" are tolerated.
func (s *Simple) Delete(ctx context.Context, resources []expv1alpha1.ManagedResource) error {
	for i := len(resources) - 1; i >= 0; i-- {
		r := resources[i]
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion(r.APIVersion)
		obj.SetKind(r.Kind)
		obj.SetNamespace(r.Namespace)
		obj.SetName(r.Name)

		opts := []client.DeleteOption{}
		if r.UID != "" {
			uid := types.UID(r.UID)
			opts = append(opts, &client.DeleteOptions{
				Preconditions: &metav1.Preconditions{UID: &uid},
			})
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
			return fmt.Errorf("delete %s/%s %s: %w", r.APIVersion, r.Kind, refName(r), err)
		}
	}
	return nil
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
func (s *Simple) applyTemplate(ctx context.Context, rt *runtime.Runtime, w watchrouter.Watcher, n *runtime.Node, desired []*unstructured.Unstructured) ([]expv1alpha1.ManagedResource, error) {
	type mapping struct {
		gvr        schema.GroupVersionResource
		namespaced bool
	}
	mappings := make([]mapping, len(desired))
	for i, obj := range desired {
		gvr, namespaced, err := s.mappingFor(n, obj)
		if err != nil {
			return nil, err
		}
		mappings[i] = mapping{gvr: gvr, namespaced: namespaced}
	}

	applied := make([]expv1alpha1.ManagedResource, 0, len(desired))
	for i, obj := range desired {
		s.defaultNamespace(rt, mappings[i].namespaced, obj)
		if err := s.watchObject(w, n.ID(), mappings[i].gvr, obj); err != nil {
			return applied, fmt.Errorf("register watch: %w", err)
		}
		if err := s.ssaApply(ctx, obj); err != nil {
			return applied, err
		}
		// SSA returned UID + server-managed fields on obj. Record the
		// identity AFTER apply succeeded so we never advertise tracking
		// for resources that didn't actually land.
		applied = append(applied, managedResourceFrom(n, obj))
	}
	return applied, nil
}

// applySubgraph runs a nested Graph node's child Program. The child Runtime
// is seeded with a snapshot of this scope so child expressions can capture
// (and shadow) parent values. The child's node outputs are published back to
// the parent scope as a map under the subgraph node's ID, making them
// addressable as ${nodeID.childNode.field}. Managed resources and unresolved
// NodeIDs from the child are returned with their IDs qualified by the
// subgraph node ID so the reconciler's tracking stays unambiguous across
// frames. The child's watches register against the same per-Graph Watcher,
// so drift on a nested resource re-enqueues the owning Graph.
func (s *Simple) applySubgraph(ctx context.Context, rt *runtime.Runtime, w watchrouter.Watcher, n *runtime.Node) ([]expv1alpha1.ManagedResource, []string, error) {
	sub := n.Spec().SubProgram
	if sub == nil {
		return nil, nil, fmt.Errorf("subgraph node has no compiled program")
	}
	childRT := runtime.New(sub, rt.Graph(),
		runtime.WithSeedScope(rt.Scope()),
		runtime.WithMaxCollectionSize(rt.MaxCollectionSize()),
	)

	childResult, applyErr := s.Apply(ctx, childRT, w)

	prefix := n.ID() + "/"
	applied := make([]expv1alpha1.ManagedResource, 0, len(childResult.Applied))
	for _, mr := range childResult.Applied {
		mr.NodeID = prefix + mr.NodeID
		applied = append(applied, mr)
	}
	unresolved := make([]string, 0, len(childResult.Unresolved))
	for _, u := range childResult.Unresolved {
		unresolved = append(unresolved, prefix+u)
	}

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

	return applied, unresolved, applyErr
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
func (s *Simple) watchObject(w watchrouter.Watcher, nodeID string, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) error {
	if w == nil {
		return nil
	}
	return w.Watch(watchrouter.WatchRequest{
		NodeID:    nodeID,
		GVR:       gvr,
		Name:      obj.GetName(),
		Namespace: obj.GetNamespace(),
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

// ssaApply server-side applies obj with the graph-engine field manager. We
// force ownership so re-applies after a hand-edit converge back.
func (s *Simple) ssaApply(ctx context.Context, obj *unstructured.Unstructured) error {
	return s.Client.Patch(ctx, obj, client.Apply, client.FieldOwner(FieldManager), client.ForceOwnership)
}
