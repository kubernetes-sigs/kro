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

package graphrevision

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/apis"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

const (
	GraphVerified = string(v1alpha1.GraphRevisionConditionTypeGraphVerified)
)

var graphRevisionConditionTypes = apis.NewReadyConditions(GraphVerified)

func (r *GraphRevisionReconciler) updateStatus(
	ctx context.Context,
	obj *v1alpha1.GraphRevision,
	topologicalOrder []string,
	resources []v1alpha1.ResourceInformation,
) error {
	// GraphRevision API status stays conditions-only. Runtime scheduling states
	// (Pending/Active/Failed) are internal to the in-memory registry and are not
	// part of the external API contract.

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &v1alpha1.GraphRevision{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
			return fmt.Errorf("failed to get current graph revision: %w", err)
		}

		dc := current.DeepCopy()
		dc.Status.Conditions = obj.Status.Conditions
		dc.Status.TopologicalOrder = topologicalOrder
		dc.Status.Resources = resources

		if equality.Semantic.DeepEqual(current.Status, dc.Status) {
			return nil
		}

		return r.Status().Patch(ctx, dc, client.MergeFrom(current))
	})
}

func (r *GraphRevisionReconciler) setManaged(ctx context.Context, obj *v1alpha1.GraphRevision) error {
	ctrl.LoggerFrom(ctx).V(1).Info("setting graphrevision as managed")

	if metadata.HasGraphRevisionFinalizer(obj) {
		return nil
	}

	dc := obj.DeepCopy()
	metadata.SetGraphRevisionFinalizer(dc)
	return r.Patch(ctx, dc, client.MergeFrom(obj))
}

func (r *GraphRevisionReconciler) setUnmanaged(ctx context.Context, obj *v1alpha1.GraphRevision) error {
	ctrl.LoggerFrom(ctx).V(1).Info("setting graphrevision as unmanaged")

	if !metadata.HasGraphRevisionFinalizer(obj) {
		return nil
	}

	dc := obj.DeepCopy()
	metadata.RemoveGraphRevisionFinalizer(dc)
	return r.Patch(ctx, dc, client.MergeFrom(obj))
}

// NewConditionsMarkerFor creates a marker to manage GraphRevision conditions.
func NewConditionsMarkerFor(o apis.Object) *ConditionsMarker {
	return &ConditionsMarker{cs: graphRevisionConditionTypes.For(o)}
}

// ConditionsMarker provides an API to mark conditions onto a GraphRevision.
type ConditionsMarker struct {
	cs apis.ConditionSet
}

func (m *ConditionsMarker) GraphVerified() {
	m.cs.SetTrueWithReason(GraphVerified, "Verified", "graph revision compiled and verified")
}

func (m *ConditionsMarker) GraphInvalid(msg string) {
	m.cs.SetFalse(GraphVerified, "InvalidGraph", msg)
}
