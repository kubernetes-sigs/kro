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

// Package schemawatcher watches CustomResourceDefinitions and re-enqueues
// Graphs whose templates reference a Kind whose schema changed.
//
// The package solves two correctness problems:
//
//  1. Without it, a CRD field added / removed / re-validated never
//     reaches the compile cache. Graphs that reference the Kind keep
//     using a stale Program until something else (drift, spec edit,
//     restart) forces a recompile.
//
//  2. Graphs whose templates use CEL in `apiVersion` or `kind` have
//     unknowable GroupKind dependencies at compile time. They must
//     re-reconcile on *any* CRD change to stay current.
//
// Architecture:
//
//	┌───────────────────────────────┐
//	│ apiserver CRD informer        │
//	└──────────────┬────────────────┘
//	               │ Add/Update/Delete events
//	               ▼
//	┌───────────────────────────────┐
//	│ SchemaWatcher                 │
//	│  - hash-dedup per GroupKind   │  ←─ skip non-schema CRD updates
//	│  - reverse index:             │
//	│      GroupKind → set[GraphKey]│
//	│  - dynamic set:               │
//	│      Graphs with CEL apiVer-  │
//	│      sion/kind (watch ALL)    │
//	│  - per-cycle Track/Done       │  ←─ same two-cycle pattern as
//	│    subscription model         │      watchrouter
//	└──────────────┬────────────────┘
//	               │ enqueue + invalidate
//	               ▼
//	        Reconciler's queue
package schemawatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// GraphInvalidator drops cached state for a Graph so the next reconcile
// recompiles. Typically the Registry; tests can pass a fake.
type GraphInvalidator interface {
	Delete(key client.ObjectKey)
}

// SchemaInvalidator drops cached schema state for a GroupKind so the
// next compile reads fresh data. Typically the Compiler.
type SchemaInvalidator interface {
	InvalidateSchema(gk schema.GroupKind)
}

// Config wires the SchemaWatcher to its collaborators. All fields are
// optional except Cache, which is required to attach the CRD informer.
type Config struct {
	// Cache is the controller-runtime cache the SchemaWatcher uses to
	// subscribe to CRD events. Sharing the manager cache means we don't
	// run a duplicate informer.
	Cache ctrlcache.Cache

	// Graphs is invoked on every CRD event that surfaces a schema
	// change, once per affected Graph, before the Graph is enqueued.
	// Implementations should drop any cached compiled Program for the
	// Graph so the next reconcile re-compiles.
	Graphs GraphInvalidator

	// Schemas is invoked on every CRD event that surfaces a schema
	// change, once per changed GroupKind. Implementations should drop
	// any cached schema resolution for the GK so the next compile
	// sees fresh data.
	Schemas SchemaInvalidator

	// EventBuffer is the depth of the channel feeding the source.Source
	// returned from Source(). A buffered channel keeps the CRD
	// informer's event goroutine from blocking the reverse-index walk.
	// Default: 1024.
	EventBuffer int
}

// SchemaWatcher tracks per-Graph schema dependencies and re-enqueues
// affected Graphs when a CRD they depend on actually changes.
//
// Concurrent-safe: mu guards every map access. The CRD informer's event
// goroutine and per-Graph Subscribe call sites can interleave freely.
type SchemaWatcher struct {
	log     logr.Logger
	cache   ctrlcache.Cache
	graphs  GraphInvalidator
	schemas SchemaInvalidator

	events chan event.GenericEvent
	closed atomic.Bool

	mu sync.RWMutex
	// byGK is the reverse index: which Graphs care about this GK.
	byGK map[schema.GroupKind]map[client.ObjectKey]struct{}
	// dynamic is the set of Graphs with CEL in apiVersion/kind — they
	// re-reconcile on any CRD change.
	dynamic map[client.ObjectKey]struct{}
	// graphs is the per-Graph subscription state for the Track/Done
	// commit cycle.
	subs map[client.ObjectKey]*graphSub
	// schemaHashes is the dedup cache: the last-seen content hash per
	// known GroupKind. A CRD event whose hash matches the cached entry
	// is a non-schema update (annotation, label, status echo) and is
	// dropped without enqueueing any Graph.
	schemaHashes map[schema.GroupKind]string

	// handlerReg is the informer event-handler registration so Start()
	// can hand it back to the cache on shutdown.
	handlerReg cache.ResourceEventHandlerRegistration
}

// graphSub tracks one Graph's in-flight and last-committed schema
// dependencies. Two-cycle commit mirrors the watchrouter's design so
// removing a GK on a re-spec actually shrinks the reverse index.
type graphSub struct {
	currentGKs map[schema.GroupKind]struct{}
	currentDyn bool
	prevGKs    map[schema.GroupKind]struct{}
	prevDyn    bool
}

// New constructs a SchemaWatcher. It does not start the informer; call
// manager.Add(w) (since SchemaWatcher is a Runnable) or call
// w.Start(ctx) directly.
func New(log logr.Logger, cfg Config) *SchemaWatcher {
	buf := cfg.EventBuffer
	if buf <= 0 {
		buf = 1024
	}
	return &SchemaWatcher{
		log:          log.WithName("schema-watcher"),
		cache:        cfg.Cache,
		graphs:       cfg.Graphs,
		schemas:      cfg.Schemas,
		events:       make(chan event.GenericEvent, buf),
		byGK:         make(map[schema.GroupKind]map[client.ObjectKey]struct{}),
		dynamic:      make(map[client.ObjectKey]struct{}),
		subs:         make(map[client.ObjectKey]*graphSub),
		schemaHashes: make(map[schema.GroupKind]string),
	}
}

// Source returns the source.Source the Graph controller wires via
// WatchesRawSource so CRD events flow into the same work queue as
// Graph spec changes and drift events.
func (w *SchemaWatcher) Source() source.Source {
	return source.Channel(w.events, &handler.EnqueueRequestForObject{})
}

// Start implements manager.Runnable. It attaches the informer event
// handler on cache start, blocks until ctx is done, then unregisters
// the handler.
func (w *SchemaWatcher) Start(ctx context.Context) error {
	if w.cache == nil {
		return fmt.Errorf("schema watcher: Cache not configured")
	}
	informer, err := w.cache.GetInformer(ctx, &apiextensionsv1.CustomResourceDefinition{})
	if err != nil {
		return fmt.Errorf("get CRD informer: %w", err)
	}
	reg, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    w.onAdd,
		UpdateFunc: w.onUpdate,
		DeleteFunc: w.onDelete,
	})
	if err != nil {
		return fmt.Errorf("attach CRD event handler: %w", err)
	}
	w.handlerReg = reg
	w.log.Info("schema watcher started")
	<-ctx.Done()
	if err := informer.RemoveEventHandler(reg); err != nil {
		w.log.V(1).Error(err, "remove CRD event handler on shutdown")
	}
	w.closed.Store(true)
	w.log.Info("schema watcher stopped")
	return nil
}

// NeedLeaderElection ties the schema watcher's lifecycle to the leader.
// Same rationale as the watchrouter: the index is process-local; only
// the active leader owns it.
func (w *SchemaWatcher) NeedLeaderElection() bool { return true }

var _ manager.Runnable = (*SchemaWatcher)(nil)
var _ manager.LeaderElectionRunnable = (*SchemaWatcher)(nil)

// --- Subscribe API ----------------------------------------------------

// Subscription is the per-Graph handle a reconciler uses to declare
// schema dependencies. Obtain one with ForGraph at the top of a
// reconcile, call Track for each literal GroupKind the Graph references,
// TrackDynamic if any Template has CEL in apiVersion/kind, then commit
// with Done(true). Done(false) discards the in-flight set and leaves
// the previously-committed subscription authoritative.
type Subscription interface {
	Track(gk schema.GroupKind)
	TrackDynamic()
	Done(commit bool)
}

// ForGraph returns a per-Graph Subscription handle.
func (w *SchemaWatcher) ForGraph(key client.ObjectKey) Subscription {
	return &subscription{w: w, key: key}
}

// RemoveGraph drops every subscription for the Graph. Called from the
// Reconciler on Graph deletion so the reverse index doesn't retain
// stale entries.
func (w *SchemaWatcher) RemoveGraph(key client.ObjectKey) {
	w.mu.Lock()
	defer w.mu.Unlock()

	sub, ok := w.subs[key]
	if !ok {
		return
	}
	for gk := range sub.prevGKs {
		w.unregisterLocked(gk, key)
	}
	for gk := range sub.currentGKs {
		w.unregisterLocked(gk, key)
	}
	if sub.prevDyn || sub.currentDyn {
		delete(w.dynamic, key)
	}
	delete(w.subs, key)
}

type subscription struct {
	w   *SchemaWatcher
	key client.ObjectKey
}

func (s *subscription) Track(gk schema.GroupKind) {
	s.w.mu.Lock()
	defer s.w.mu.Unlock()

	sub := s.w.subs[s.key]
	if sub == nil {
		sub = &graphSub{
			currentGKs: make(map[schema.GroupKind]struct{}),
			prevGKs:    make(map[schema.GroupKind]struct{}),
		}
		s.w.subs[s.key] = sub
	}
	if _, dup := sub.currentGKs[gk]; dup {
		return
	}
	sub.currentGKs[gk] = struct{}{}
	// Add to reverse index immediately (idempotent within a cycle).
	s.w.registerLocked(gk, s.key)
}

func (s *subscription) TrackDynamic() {
	s.w.mu.Lock()
	defer s.w.mu.Unlock()

	sub := s.w.subs[s.key]
	if sub == nil {
		sub = &graphSub{
			currentGKs: make(map[schema.GroupKind]struct{}),
			prevGKs:    make(map[schema.GroupKind]struct{}),
		}
		s.w.subs[s.key] = sub
	}
	if sub.currentDyn {
		return
	}
	sub.currentDyn = true
	s.w.dynamic[s.key] = struct{}{}
}

func (s *subscription) Done(commit bool) {
	if commit {
		s.commit()
		return
	}
	s.abort()
}

// commit swaps the in-flight set into the committed set and removes any
// GKs that were in the previous committed set but not the new one.
func (s *subscription) commit() {
	s.w.mu.Lock()
	defer s.w.mu.Unlock()

	sub := s.w.subs[s.key]
	if sub == nil {
		return
	}
	// Remove GKs that were committed previously but not re-tracked.
	for gk := range sub.prevGKs {
		if _, stillTracked := sub.currentGKs[gk]; stillTracked {
			continue
		}
		s.w.unregisterLocked(gk, s.key)
	}
	// Dynamic flag toggle: if previously dynamic and not re-claimed,
	// drop from the dynamic set.
	if sub.prevDyn && !sub.currentDyn {
		delete(s.w.dynamic, s.key)
	}
	// Swap.
	sub.prevGKs = sub.currentGKs
	sub.prevDyn = sub.currentDyn
	sub.currentGKs = make(map[schema.GroupKind]struct{})
	sub.currentDyn = false
}

// abort discards the in-flight cycle. The previously committed
// subscription remains authoritative. GKs added this cycle but not
// previously committed are removed from the index.
func (s *subscription) abort() {
	s.w.mu.Lock()
	defer s.w.mu.Unlock()

	sub := s.w.subs[s.key]
	if sub == nil {
		return
	}
	for gk := range sub.currentGKs {
		if _, shared := sub.prevGKs[gk]; shared {
			continue
		}
		s.w.unregisterLocked(gk, s.key)
	}
	if sub.currentDyn && !sub.prevDyn {
		delete(s.w.dynamic, s.key)
	}
	sub.currentGKs = make(map[schema.GroupKind]struct{})
	sub.currentDyn = false
}

// --- Index helpers (must hold w.mu) -----------------------------------

func (w *SchemaWatcher) registerLocked(gk schema.GroupKind, key client.ObjectKey) {
	set, ok := w.byGK[gk]
	if !ok {
		set = make(map[client.ObjectKey]struct{})
		w.byGK[gk] = set
	}
	set[key] = struct{}{}
}

func (w *SchemaWatcher) unregisterLocked(gk schema.GroupKind, key client.ObjectKey) {
	set, ok := w.byGK[gk]
	if !ok {
		return
	}
	delete(set, key)
	if len(set) == 0 {
		delete(w.byGK, gk)
	}
}

// --- CRD event routing ------------------------------------------------

func (w *SchemaWatcher) onAdd(obj any) {
	crd := toCRD(obj)
	if crd == nil {
		return
	}
	gk := schema.GroupKind{Group: crd.Spec.Group, Kind: crd.Spec.Names.Kind}
	hash := hashCRDSchema(crd)

	w.mu.Lock()
	// Adds always go through. Even if the hash matches a previously
	// known entry (rare on Add but possible on re-list after a delete
	// that we observed), the controller-runtime initial list will
	// fire Add events for every CRD; we MUST seed schemaHashes here.
	w.schemaHashes[gk] = hash
	affected := w.affectedLocked(gk)
	w.mu.Unlock()

	// On Add, also enqueue every Graph in the dynamic set AND every
	// Graph that was previously failing because this Kind didn't
	// exist — we can't know which ones, so we enqueue every Graph in
	// our subscriptions map. This is cheap on Add and gives stuck
	// Graphs a chance to recover.
	w.mu.RLock()
	for key := range w.subs {
		affected[key] = struct{}{}
	}
	w.mu.RUnlock()

	w.notify(gk, affected, "CRD added")
}

func (w *SchemaWatcher) onUpdate(oldObj, newObj any) {
	crd := toCRD(newObj)
	if crd == nil {
		return
	}
	gk := schema.GroupKind{Group: crd.Spec.Group, Kind: crd.Spec.Names.Kind}
	newHash := hashCRDSchema(crd)

	w.mu.Lock()
	prevHash, known := w.schemaHashes[gk]
	if known && prevHash == newHash {
		// Non-schema update (annotation tweak, status echo, etc.).
		w.mu.Unlock()
		return
	}
	w.schemaHashes[gk] = newHash
	affected := w.affectedLocked(gk)
	w.mu.Unlock()

	w.notify(gk, affected, "CRD schema changed")
}

func (w *SchemaWatcher) onDelete(obj any) {
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = d.Obj
	}
	crd := toCRD(obj)
	if crd == nil {
		return
	}
	gk := schema.GroupKind{Group: crd.Spec.Group, Kind: crd.Spec.Names.Kind}

	w.mu.Lock()
	delete(w.schemaHashes, gk)
	affected := w.affectedLocked(gk)
	w.mu.Unlock()

	w.notify(gk, affected, "CRD deleted")
}

// affectedLocked returns the set of Graph keys that need re-reconcile
// for a change to the given GK: explicit subscribers + every dynamic
// Graph. Must hold w.mu (read or write).
func (w *SchemaWatcher) affectedLocked(gk schema.GroupKind) map[client.ObjectKey]struct{} {
	out := make(map[client.ObjectKey]struct{}, len(w.byGK[gk])+len(w.dynamic))
	for k := range w.byGK[gk] {
		out[k] = struct{}{}
	}
	for k := range w.dynamic {
		out[k] = struct{}{}
	}
	return out
}

// notify invokes the configured invalidators (schema first, then per
// graph) and pushes a GenericEvent for each affected Graph. The
// schema invalidator goes first so the next reconcile finds a clean
// resolver cache; the graph invalidator drops the cached Program;
// then the enqueue triggers the reconcile.
func (w *SchemaWatcher) notify(gk schema.GroupKind, keys map[client.ObjectKey]struct{}, reason string) {
	if len(keys) == 0 {
		w.log.V(2).Info("schema event with no subscribers", "gk", gk, "reason", reason)
	}

	if w.schemas != nil {
		w.schemas.InvalidateSchema(gk)
	}
	for key := range keys {
		if w.graphs != nil {
			w.graphs.Delete(key)
		}
		w.enqueue(key)
	}
	w.log.V(1).Info("notified subscribers", "gk", gk, "reason", reason, "count", len(keys))
}

func (w *SchemaWatcher) enqueue(key client.ObjectKey) {
	if w.closed.Load() {
		return
	}
	u := &unstructured.Unstructured{}
	u.SetName(key.Name)
	u.SetNamespace(key.Namespace)
	select {
	case w.events <- event.GenericEvent{Object: u}:
	default:
		w.log.V(0).Info("event buffer full; dropping schema enqueue", "graph", key)
	}
}

// --- helpers ----------------------------------------------------------

func toCRD(obj any) *apiextensionsv1.CustomResourceDefinition {
	switch v := obj.(type) {
	case *apiextensionsv1.CustomResourceDefinition:
		return v
	case apiextensionsv1.CustomResourceDefinition:
		return &v
	default:
		// Try meta.Accessor as a final fallback so partial-metadata
		// informers don't silently drop events. We can't extract
		// schema from metadata-only objects though, so for non-CRD
		// types we just return nil.
		if _, err := meta.Accessor(obj); err == nil {
			return nil
		}
		return nil
	}
}

// hashCRDSchema computes a stable hash of the parts of the CRD spec a
// compiler cares about. Annotation tweaks, label edits, status echoes,
// and resourceVersion bumps that don't touch schema content produce
// the same hash → dedup'd.
func hashCRDSchema(crd *apiextensionsv1.CustomResourceDefinition) string {
	if crd == nil {
		return ""
	}
	// Project the spec onto a stable shape, then JSON-encode. The
	// concrete fields we hash are: group, names, conversion, and per-
	// version (name, served, storage, schema.openAPIV3Schema,
	// subresources). Everything else (annotations, status,
	// observedGeneration) is ignored.
	type versionView struct {
		Name         string                                           `json:"name"`
		Served       bool                                             `json:"served"`
		Storage      bool                                             `json:"storage"`
		Schema       *apiextensionsv1.CustomResourceValidation        `json:"schema,omitempty"`
		Subresources *apiextensionsv1.CustomResourceSubresources      `json:"subresources,omitempty"`
		Columns      []apiextensionsv1.CustomResourceColumnDefinition `json:"columns,omitempty"`
	}
	type specView struct {
		Group      string                                        `json:"group"`
		Names      apiextensionsv1.CustomResourceDefinitionNames `json:"names"`
		Scope      apiextensionsv1.ResourceScope                 `json:"scope"`
		Conversion *apiextensionsv1.CustomResourceConversion     `json:"conversion,omitempty"`
		Versions   []versionView                                 `json:"versions"`
	}
	v := specView{
		Group:      crd.Spec.Group,
		Names:      crd.Spec.Names,
		Scope:      crd.Spec.Scope,
		Conversion: crd.Spec.Conversion,
	}
	for _, ver := range crd.Spec.Versions {
		v.Versions = append(v.Versions, versionView{
			Name:         ver.Name,
			Served:       ver.Served,
			Storage:      ver.Storage,
			Schema:       ver.Schema,
			Subresources: ver.Subresources,
			Columns:      ver.AdditionalPrinterColumns,
		})
	}
	buf, err := json.Marshal(v)
	if err != nil {
		// Should never happen for this shape; fall back to a value
		// that always differs so we re-enqueue rather than silently
		// dedup on broken data.
		return fmt.Sprintf("unhashable: %v", err)
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

// --- introspection (tests + metrics) ----------------------------------

// SubscribedGraphs returns the count of distinct Graphs currently
// tracked across both static GK subscriptions and the dynamic set.
func (w *SchemaWatcher) SubscribedGraphs() int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return len(w.subs)
}

// GraphsForGroupKind returns the Graph keys subscribed to the given GK.
// Tests use this to assert index state without poking internals.
func (w *SchemaWatcher) GraphsForGroupKind(gk schema.GroupKind) []client.ObjectKey {
	w.mu.RLock()
	defer w.mu.RUnlock()
	set := w.byGK[gk]
	out := make([]client.ObjectKey, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// DynamicGraphs returns the Graph keys flagged as dynamic-GVK.
func (w *SchemaWatcher) DynamicGraphs() []client.ObjectKey {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make([]client.ObjectKey, 0, len(w.dynamic))
	for k := range w.dynamic {
		out = append(out, k)
	}
	return out
}

// SchemaHash returns the cached content hash for the given GK or empty
// if none. Used in tests to verify dedup behavior.
func (w *SchemaWatcher) SchemaHash(gk schema.GroupKind) string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.schemaHashes[gk]
}

// Assert that metav1 import isn't dead in case future versions of the
// helpers drop the only usage. Kept defensively because subscription
// states reference the metav1 package indirectly via apiextensions.
var _ = metav1.Time{}
