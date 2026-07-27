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

// Package executor converges the cluster toward the desired state encoded
// in a runtime.Runtime. Implementations differ in how they track ownership
// (none, labels, ApplySet), wait for readiness, and prune drift. The Graph
// reconciler consumes whichever implementation is wired in.
package executor

import (
	"context"
	"errors"

	expv1alpha1 "sigs.k8s.io/krocodile/api/v1alpha1"
	"sigs.k8s.io/krocodile/pkg/runtime"
	"sigs.k8s.io/krocodile/pkg/watchrouter"
)

// FieldManager is the field-manager identity all SSA writes use. Keep it
// stable across versions so re-applies don't fight prior writes.
const FieldManager = "krocodile-controller"

// ErrUnsupported is returned when an executor refuses to handle a node kind
// it does not implement yet (e.g. ref/watch in v1).
var ErrUnsupported = errors.New("executor: node kind not supported")

// ErrNotReady is the sentinel an executor returns when one or more nodes
// applied successfully but their readyWhen conditions evaluate false. The
// reconciler treats this as a soft "requeue and retry" rather than a hard
// error — the user's spec is fine, the cluster just hasn't converged yet.
var ErrNotReady = errors.New("executor: node not ready")

// ApplyResult records what an Apply call observed. The Reconciler uses
// it to drive the prune set: Applied lists entries whose identities we
// know this cycle; Unresolved lists NodeIDs whose Resolve hit data-
// pending (we can't enumerate their identities, so the Reconciler keeps
// their previous entries as "uncertain — still wanted").
//
// Order matters: Applied is appended in topological apply order so
// reverse iteration gives reverse-apply order on prune.
type ApplyResult struct {
	Applied    []expv1alpha1.ManagedResource
	Unresolved []string // NodeIDs whose Resolve hit data-pending this cycle
}

// Interface is the cluster-I/O surface used by the Graph reconciler.
// Implementations should treat Apply as idempotent and Delete as tolerant
// of partial prior progress (NotFound is success).
type Interface interface {
	// Apply walks the runtime in topological order, resolves each node's
	// desired state, registers a watch with the supplied Watcher, and
	// applies the resource to the cluster.
	//
	// Soft errors (ErrDataPending from Resolve / IsIgnored,
	// ErrWaitingForReadiness from CheckReadiness) do not abort the walk
	// — they are recorded and returned wrapped as ErrNotReady, while
	// the walk continues so every reachable node still declares its
	// watch and contributes its identities to Applied. Hard errors
	// (apply 5xx, type errors, unsupported kinds) abort immediately;
	// Applied carries whatever was tracked up to the abort point.
	//
	// The Watcher is the per-Graph handle obtained from the dynamic
	// controller; pass watchrouter.NoopWatcher{} when drift
	// detection is not desired (CLI / dry runs).
	Apply(ctx context.Context, rt *runtime.Runtime, w watchrouter.Watcher) (ApplyResult, error)

	// Delete removes the supplied managed resources from the cluster in
	// the reverse of the slice's order. UID precondition is applied per
	// entry so we never remove an impostor that replaced our resource
	// out of band. NotFound is tolerated. Safe to call repeatedly.
	//
	// Unlike Apply, Delete does not consult the runtime — it operates
	// purely from the persisted tracking record. This means a Graph
	// whose spec was edited (template renamed, forEach shrunk, node
	// removed) can still be deleted cleanly: the record knows what was
	// applied, regardless of what the current spec would re-derive.
	Delete(ctx context.Context, resources []expv1alpha1.ManagedResource) error
}
