// Copyright 2025 The Kube Resource Orchestrator Authors
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

package graph

import (
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	krotruntime "github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// resourceKey is the identity tuple used to dedup ManagedResource
// entries. UID is excluded because pre-apply entries (write-ahead)
// and post-apply entries (with UID) describe the same resource.
//
// Identity keys on GROUP + Kind, not the full apiVersion: a CRD's multiple
// served versions all address the SAME stored object, so a version-only
// template change (e.g. apps/v1 -> apps/v2 for the same Kind/namespace/name)
// must NOT make the old-version entry look like a different resource. If it
// did, the old entry would become a prune candidate and Delete — which keys on
// the stable object UID — would delete the very object just applied under the
// new version (a destructive apply-then-prune churn on one object). Keying on
// Group+Kind makes the two versions dedup to one identity.
type resourceKey struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
}

func keyOf(r expv1alpha1.ManagedResource) resourceKey {
	// APIVersion is "group/version" (or just "version" for core); take the group
	// so the version segment does not participate in identity. A parse error
	// (malformed apiVersion) falls back to an empty group, which still yields a
	// stable key for that (malformed) entry.
	group := schema.FromAPIVersionAndKind(r.APIVersion, r.Kind).Group
	return resourceKey{
		Group:     group,
		Kind:      r.Kind,
		Namespace: r.Namespace,
		Name:      r.Name,
	}
}

// diffManagedResources compares the previously-tracked set against the
// just-applied set, accounting for nodes whose identities couldn't be
// resolved this cycle.
//
// Returned newSet is the set the controller wants to advertise after a
// fully-successful Apply (Applied entries + entries preserved from
// previous because their NodeID hit data-pending). pruneCandidates are
// previous entries we're confident are no longer wanted — node dropped
// from spec, forEach shrunk, rename, or includeWhen flipped to false.
//
// Order: newSet preserves the executor's topological apply order from
// result.Applied; preserved entries are appended after, in their
// previous-cycle order. Reverse iteration over newSet still gives a
// reasonable reverse-apply order for delete.
func diffManagedResources(
	previous []expv1alpha1.ManagedResource,
	result executor.ApplyResult,
) ([]expv1alpha1.ManagedResource, []expv1alpha1.ManagedResource) {
	unresolved := make(map[string]struct{}, len(result.Unresolved))
	for _, nodeID := range result.Unresolved {
		unresolved[nodeID] = struct{}{}
	}

	applied := make(map[resourceKey]expv1alpha1.ManagedResource, len(result.Applied))
	for _, r := range result.Applied {
		applied[keyOf(r)] = r
	}

	newSet := make([]expv1alpha1.ManagedResource, 0, len(result.Applied)+len(previous))
	newSet = append(newSet, result.Applied...)

	var pruneCandidates []expv1alpha1.ManagedResource

	for _, prev := range previous {
		if _, alreadyApplied := applied[keyOf(prev)]; alreadyApplied {
			continue
		}
		if _, isUnresolved := unresolved[prev.NodeID]; isUnresolved {
			newSet = append(newSet, prev)
			continue
		}
		pruneCandidates = append(pruneCandidates, prev)
	}
	return newSet, pruneCandidates
}

// intendedManagedResources projects the resource identities a runtime is about
// to apply this cycle, best-effort and without cluster writes. It is
// intentionally lossy (data-pending/ignored/unresolvable nodes are skipped) and
// UID-free: the reconciler write-aheads this pre-apply superset so a lost status
// write after Apply still leaves teardown something to delete; keyOf excludes
// UID so post-apply entries dedup against their intent entry. Subgraph nodes are
// recursed to arbitrary depth, qualifying child NodeIDs with the subgraph prefix
// exactly as the executor's applySubgraph does so intent and post-apply entries
// dedup on identity.
func intendedManagedResources(rt *krotruntime.Runtime) []expv1alpha1.ManagedResource {
	if rt == nil {
		return nil
	}
	var out []expv1alpha1.ManagedResource
	seen := make(map[resourceKey]struct{})
	projectManagedResources(rt, "", seen, &out)
	return out
}

// projectManagedResources walks one runtime frame, appending template-node
// identities (deduped via seen) to out and recursing into subgraph frames.
// prefix is the frame's qualified node-ID prefix ("" at root), matching the
// executor's applySubgraph qualification.
func projectManagedResources(
	rt *krotruntime.Runtime,
	prefix string,
	seen map[resourceKey]struct{},
	out *[]expv1alpha1.ManagedResource,
) {
	seedDefNodes(rt)

	for _, n := range rt.Nodes() {
		// A subgraph node has no payload of its own; recurse into its child frame,
		// qualifying node IDs with this node's prefix — as applySubgraph does.
		if n.Kind() == compiler.NodeKindGraph {
			if child := childRuntime(rt, n); child != nil {
				projectManagedResources(child, prefix+n.ID()+"/", seen, out)
			}
			continue
		}
		// Only template nodes produce owned/torn-down resources (ref = read-only,
		// patch = tracked as contributions, def = no I/O).
		if n.Kind() != compiler.NodeKindTemplate {
			continue
		}
		// includeWhen:false won't be applied; an IsIgnored error means we can't
		// decide yet — keep it out (its post-apply Applied entry records it).
		if ignored, err := n.IsIgnored(); err == nil && ignored {
			continue
		}
		desired, err := n.Resolve()
		if err != nil {
			continue
		}
		for _, obj := range desired {
			gvk := obj.GroupVersionKind()
			if gvk.Kind == "" || obj.GetName() == "" {
				continue
			}
			// Namespace-default exactly as the executor does before apply: a
			// namespaced object with no explicit namespace lands in the Graph's
			// namespace, so the intent entry (ns=graph) dedups against the applied
			// entry instead of rewriting status every cycle.
			ns := obj.GetNamespace()
			if ns == "" && n.Namespaced() {
				ns = rt.Graph().GetNamespace()
			}
			// A dynamic-GVK node has no compile-time REST scope, so a ns="" object
			// can't be namespace-defaulted here (the executor resolves scope from the
			// live RESTMapper at apply). Emitting a ns="" intent entry would never
			// dedup against the applied entry, rewriting status every cycle — skip it;
			// its post-apply Applied entry records it correctly. (A dynamic node with
			// an explicit namespace keeps its intent entry and dedups.)
			if ns == "" && n.DynamicGVK() {
				continue
			}
			mr := expv1alpha1.ManagedResource{
				NodeID:     prefix + n.ID(),
				APIVersion: gvk.GroupVersion().String(),
				Kind:       gvk.Kind,
				Namespace:  ns,
				Name:       obj.GetName(),
			}
			k := keyOf(mr)
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			*out = append(*out, mr)
		}
	}
}

// seedDefNodes seeds every Def node of rt into rt's scope so downstream
// template/patch expressions that reference them (e.g. `schema.spec...`) can
// resolve in memory. Idempotent — safe to call more than once on a frame.
func seedDefNodes(rt *krotruntime.Runtime) {
	for _, n := range rt.Nodes() {
		if n.Kind() != compiler.NodeKindDef {
			continue
		}
		desired, err := n.Resolve()
		if err != nil || len(desired) == 0 {
			continue
		}
		n.SetObserved(desired, desired)
		if n.IsCollection() {
			list := make([]any, 0, len(desired))
			for _, obj := range desired {
				list = append(list, obj.Object)
			}
			rt.Set(n.ID(), list)
		} else {
			rt.Set(n.ID(), desired[0].Object)
		}
	}
}

// childRuntime builds the child runtime for a subgraph (NodeKindGraph) node the
// SAME way the executor's applySubgraph does: seed the child scope from the
// parent runtime's scope and carry MaxCollectionSize. Returns nil when the node
// carries no compiled child program (a malformed subgraph the executor would
// itself reject), so callers simply skip it — its identities are uncertain and
// its post-apply entries, if any, record it.
func childRuntime(rt *krotruntime.Runtime, n *krotruntime.Node) *krotruntime.Runtime {
	sub := n.Spec().SubProgram
	if sub == nil {
		return nil
	}
	return krotruntime.New(sub, rt.Graph(),
		krotruntime.WithSeedScope(rt.Scope()),
		krotruntime.WithMaxCollectionSize(rt.MaxCollectionSize()),
	)
}

// intendedContributions projects the patch-contribution identities a runtime is
// about to apply this cycle, best-effort and without cluster writes — the patch
// twin of intendedManagedResources. The reconciler write-aheads this so a crash
// between Apply (which mutates a patch target) and persistContributions still
// leaves Release a superset, instead of a contributed field stranded with no
// ledger entry.
//
// The projected FieldManager MUST equal what the executor applies under, or the
// write-ahead entry would never correlate with the later Release. Both derive it
// from the shared executor.PatchFieldManager(graphUID, qualifiedNodeID), so they
// cannot drift — which is why subgraphs are recursed here (to reproduce the
// executor's prefix-qualified path). Intentionally lossy: unresolvable, ignored,
// or dynamic-GVK-no-namespace patch nodes are skipped.
func intendedContributions(rt *krotruntime.Runtime) []executor.Contribution {
	if rt == nil {
		return nil
	}
	graphUID := rt.Graph().GetUID()
	var out []executor.Contribution
	seen := make(map[contribKey]struct{})
	projectContributions(rt, "", graphUID, seen, &out)
	return out
}

// projectContributions walks one runtime frame, appending patch-node
// contributions (deduped via seen) to out and recursing into subgraph frames.
// prefix is the frame's qualified node-ID prefix ("" at root); graphUID is
// threaded so the FieldManager derivation matches the executor at every depth.
func projectContributions(
	rt *krotruntime.Runtime,
	prefix string,
	graphUID types.UID,
	seen map[contribKey]struct{},
	out *[]executor.Contribution,
) {
	// Seed Def nodes into scope so patch targets referencing them (e.g.
	// `schema.spec...`) resolve in memory. Idempotent with any prior seeding.
	seedDefNodes(rt)

	for _, n := range rt.Nodes() {
		// A subgraph node has no payload of its own; recurse into its child frame,
		// qualifying node IDs with this node's prefix so the projected field manager
		// uses the SAME qualified path the child executor will.
		if n.Kind() == compiler.NodeKindGraph {
			if child := childRuntime(rt, n); child != nil {
				projectContributions(child, prefix+n.ID()+"/", graphUID, seen, out)
			}
			continue
		}
		if n.Kind() != compiler.NodeKindPatch {
			continue
		}
		if ignored, err := n.IsIgnored(); err == nil && ignored {
			continue
		}
		desired, err := n.Resolve()
		if err != nil || len(desired) != 1 {
			continue
		}
		obj := desired[0]
		gvk := obj.GroupVersionKind()
		if gvk.Kind == "" || obj.GetName() == "" {
			continue
		}
		ns := obj.GetNamespace()
		if ns == "" && n.Namespaced() {
			ns = rt.Graph().GetNamespace()
		}
		// Same dynamic-GVK-no-namespace ambiguity as intendedManagedResources:
		// the scope isn't known until apply resolves it from the RESTMapper, so a
		// ns="" entry would never correlate. Skip it.
		if ns == "" && n.DynamicGVK() {
			continue
		}
		c := executor.Contribution{
			APIVersion:   gvk.GroupVersion().String(),
			Kind:         gvk.Kind,
			Namespace:    ns,
			Name:         obj.GetName(),
			Subresource:  n.Subresource(),
			FieldManager: executor.PatchFieldManager(graphUID, prefix+n.ID()),
		}
		k := contribKeyOf(c)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		*out = append(*out, c)
	}
}

// unionManagedResources merges previous and applied, deduping on identity.
// Used after a soft or hard Apply error, where the diff isn't trustworthy
// enough to prune, so status is widened to cover everything we know about.
// Order keeps previous first, then applied entries previous didn't have.
//
// Dedup is UID-aware: keyOf excludes UID (so a UID-free write-ahead intent entry
// dedups against its applied counterpart), but on a key collision the surviving
// entry keeps whichever side's UID is set, and a later UID overrides an earlier
// one. Every caller passes the UID-free previous/intent set first, so plain
// first-wins would strand a resource in status with no UID — which Simple.Delete
// then refuses to delete, leaking it on teardown.
func unionManagedResources(
	previous []expv1alpha1.ManagedResource,
	applied []expv1alpha1.ManagedResource,
) []expv1alpha1.ManagedResource {
	idx := make(map[resourceKey]int, len(previous)+len(applied))
	out := make([]expv1alpha1.ManagedResource, 0, len(previous)+len(applied))
	add := func(r expv1alpha1.ManagedResource) {
		k := keyOf(r)
		if i, dup := idx[k]; dup {
			// Already tracked. Take this entry's UID when set, so a UID-free
			// entry adopts a real UID and a later UID wins on recreate.
			if r.UID != "" {
				out[i].UID = r.UID
			}
			return
		}
		idx[k] = len(out)
		out = append(out, r)
	}
	for _, r := range previous {
		add(r)
	}
	for _, r := range applied {
		add(r)
	}
	return out
}

// contribKey is the identity tuple for a patch contribution. The field
// manager alone is stable per patch node, but the target identity is included
// so a patch whose target name changed releases the old target's fields.
type contribKey struct {
	FieldManager string
	APIVersion   string
	Kind         string
	Namespace    string
	Name         string
	Subresource  string
}

func contribKeyOf(c executor.Contribution) contribKey {
	return contribKey{
		FieldManager: c.FieldManager,
		APIVersion:   c.APIVersion,
		Kind:         c.Kind,
		Namespace:    c.Namespace,
		Name:         c.Name,
		Subresource:  c.Subresource,
	}
}

// ReadContributions parses the persisted patch-contribution inventory off an
// object's annotation. A missing or empty annotation is an empty inventory.
func ReadContributions(obj metav1.Object) ([]executor.Contribution, error) {
	if obj == nil || obj.GetAnnotations() == nil {
		return nil, nil
	}
	raw := obj.GetAnnotations()[metadata.PatchContributionsAnnotation]
	if raw == "" {
		return nil, nil
	}
	var out []executor.Contribution
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf(
			"unmarshal patch contributions from annotation %q: %w",
			metadata.PatchContributionsAnnotation,
			err,
		)
	}
	return out, nil
}

// MarshalContributions renders a contribution inventory as its annotation
// value. An empty inventory renders as "" so the annotation can be dropped.
func MarshalContributions(contribs []executor.Contribution) (string, error) {
	if len(contribs) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(contribs)
	if err != nil {
		return "", fmt.Errorf("marshal patch contributions: %w", err)
	}
	return string(raw), nil
}

// toAPIContributions converts the in-memory executor contribution inventory
// to its persisted API representation for the Graph status subresource.
// Nil-safe: a nil/empty input returns nil so an empty inventory serializes as
// an absent field rather than an empty array.
func toAPIContributions(contribs []executor.Contribution) []expv1alpha1.Contribution {
	if len(contribs) == 0 {
		return nil
	}
	out := make([]expv1alpha1.Contribution, 0, len(contribs))
	for _, c := range contribs {
		out = append(out, expv1alpha1.Contribution{
			APIVersion:   c.APIVersion,
			Kind:         c.Kind,
			Namespace:    c.Namespace,
			Name:         c.Name,
			Subresource:  c.Subresource,
			FieldManager: c.FieldManager,
		})
	}
	return out
}

// fromAPIContributions converts the persisted API contribution inventory back
// to the in-memory executor representation the reconciler and executor use.
// Nil-safe: a nil/empty input returns nil.
func fromAPIContributions(contribs []expv1alpha1.Contribution) []executor.Contribution {
	if len(contribs) == 0 {
		return nil
	}
	out := make([]executor.Contribution, 0, len(contribs))
	for _, c := range contribs {
		out = append(out, executor.Contribution{
			APIVersion:   c.APIVersion,
			Kind:         c.Kind,
			Namespace:    c.Namespace,
			Name:         c.Name,
			Subresource:  c.Subresource,
			FieldManager: c.FieldManager,
		})
	}
	return out
}

// DiffContributions returns the entries present in prior but absent from
// current — the contributions to release because their patch node was removed
// or its target identity changed.
func DiffContributions(prior, current []executor.Contribution) []executor.Contribution {
	cur := make(map[contribKey]struct{}, len(current))
	for _, c := range current {
		cur[contribKeyOf(c)] = struct{}{}
	}
	released := make([]executor.Contribution, 0, len(prior))
	for _, p := range prior {
		if _, ok := cur[contribKeyOf(p)]; !ok {
			released = append(released, p)
		}
	}
	return released
}

// UnionContributions concatenates prior and current, deduping on identity.
// Used when an Apply hit a soft or hard error — the diff isn't trustworthy,
// so the persisted inventory widens to cover everything known.
func UnionContributions(prior, current []executor.Contribution) []executor.Contribution {
	seen := make(map[contribKey]struct{}, len(prior)+len(current))
	out := make([]executor.Contribution, 0, len(prior)+len(current))
	add := func(c executor.Contribution) {
		k := contribKeyOf(c)
		if _, dup := seen[k]; dup {
			return
		}
		seen[k] = struct{}{}
		out = append(out, c)
	}
	for _, c := range prior {
		add(c)
	}
	for _, c := range current {
		add(c)
	}
	return out
}
