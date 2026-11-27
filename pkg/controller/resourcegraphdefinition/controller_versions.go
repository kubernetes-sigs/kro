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

package resourcegraphdefinition

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	kroclient "github.com/kubernetes-sigs/kro/pkg/client"
	"github.com/kubernetes-sigs/kro/pkg/features"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// crdFailureReason maps a CRD ensure failure to a status condition reason.
// Everything kro cannot attribute keeps the generic "Failed" reason that
// KindUnready writes.
func crdFailureReason(err error) string {
	switch {
	case errors.Is(err, kroclient.ErrVersionRemovalBlocked):
		return "VersionRemovalBlocked"
	case errors.Is(err, kroclient.ErrSpokeSchemaMismatch):
		return "SpokeSchemaMismatch"
	default:
		return "Failed"
	}
}

// versionsInUse returns the instance API versions that graph revisions still
// referenced by instances were compiled against. A version in this set may not
// stop being served: instances pinned to those revisions would lose the schema
// they reconcile under.
//
// The referenced revisions come from the parent informer's revision index.
// Right after a restart no controller is registered for the RGD yet, so the
// instances are listed once from the API server instead, reading the same
// three sources the index does.
func (r *ResourceGraphDefinitionReconciler) versionsInUse(
	ctx context.Context,
	rgd *v1alpha1.ResourceGraphDefinition,
	crd *v1.CustomResourceDefinition,
) (sets.Set[string], error) {
	revisionNumbers, err := r.referencedRevisions(ctx, rgd, crd)
	if err != nil {
		return nil, err
	}

	versions := sets.New[string]()
	for revision := range revisionNumbers {
		entry, ok := r.revisionsRegistry.Get(rgd.Name, revision)
		if !ok || entry.RGDSpec == nil || entry.RGDSpec.Schema == nil {
			continue
		}
		versions.Insert(entry.RGDSpec.Schema.APIVersion)
	}
	return versions, nil
}

// referencedRevisions returns the graph revisions instances of this RGD
// currently reference.
func (r *ResourceGraphDefinitionReconciler) referencedRevisions(
	ctx context.Context,
	rgd *v1alpha1.ResourceGraphDefinition,
	crd *v1.CustomResourceDefinition,
) (sets.Set[int64], error) {
	if gvr, ok := r.registeredGVR(rgd.UID); ok {
		if refs, ok := r.dynamicController.ReferencedGraphRevisions(gvr); ok {
			return refs, nil
		}
	}

	// No warm informer: list the instances once through the storage version
	// the cluster currently serves.
	existing, err := r.crdManager.Get(ctx, crd.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return sets.New[int64](), nil
		}
		return nil, fmt.Errorf("getting CRD %s: %w", crd.Name, err)
	}
	instances, err := r.listInstances(ctx, existing)
	if err != nil {
		return nil, err
	}

	refs := sets.New[int64]()
	for i := range instances {
		for _, revision := range instanceRevisionRefs(&instances[i]) {
			refs.Insert(revision)
		}
	}
	return refs, nil
}

// listInstances lists every instance of a CRD through its storage version.
func (r *ResourceGraphDefinitionReconciler) listInstances(
	ctx context.Context,
	crd *v1.CustomResourceDefinition,
) ([]unstructured.Unstructured, error) {
	storage := ""
	for _, version := range crd.Spec.Versions {
		if version.Storage {
			storage = version.Name
			break
		}
	}
	if storage == "" {
		return nil, nil
	}

	gvr := schema.GroupVersionResource{
		Group:    crd.Spec.Group,
		Version:  storage,
		Resource: crd.Spec.Names.Plural,
	}
	list, err := r.clientSet.Dynamic().Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing instances of %s: %w", gvr, err)
	}
	return list.Items, nil
}

// revisionFloor returns the highest graph revision an instance of this RGD
// holds from a deleted RGD of the same name, so that number is never reissued.
func (r *ResourceGraphDefinitionReconciler) revisionFloor(
	ctx context.Context,
	rgd *v1alpha1.ResourceGraphDefinition,
) (int64, error) {
	if !features.FeatureGate.Enabled(features.GraphRevisionPinning) {
		return 0, nil
	}
	// With a registered controller this RGD issued every number its instances hold.
	if _, ok := r.registeredGVR(rgd.UID); ok {
		return 0, nil
	}

	crdName := extractCRDName(rgd.Spec.Schema)
	crd, err := r.crdManager.Get(ctx, crdName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("getting CRD %s: %w", crdName, err)
	}
	if !metadata.IsKROOwned(crd) || crd.Labels[metadata.ResourceGraphDefinitionNameLabel] != rgd.Name {
		return 0, nil
	}
	instances, err := r.listInstances(ctx, crd)
	if err != nil {
		return 0, err
	}

	// Only the revision an instance ran counts: a pin can name any number.
	var floor int64
	for i := range instances {
		if revision, ok := statusRevision(&instances[i], "graphRevision"); ok {
			floor = max(floor, revision)
		}
	}
	return floor, nil
}

// statusRevision reads a revision number kro wrote onto an instance's status.
func statusRevision(inst *unstructured.Unstructured, field string) (int64, bool) {
	status, ok := inst.Object["status"].(map[string]any)
	if !ok {
		return 0, false
	}
	switch value := status[field].(type) {
	case int64:
		return value, true
	case float64:
		return int64(value), true
	default:
		return 0, false
	}
}

// instanceRevisionRefs returns the revisions one instance references: the
// revision it reconciled against, the revision it targets, and its pin
// annotation when that names a concrete revision.
func instanceRevisionRefs(inst *unstructured.Unstructured) []int64 {
	var refs []int64
	for _, key := range []string{"graphRevision", "targetGraphRevision"} {
		if revision, ok := statusRevision(inst, key); ok {
			refs = append(refs, revision)
		}
	}
	if pin := inst.GetAnnotations()[v1alpha1.InstanceGraphRevisionAnnotation]; pin != "" {
		if revision, err := strconv.ParseInt(pin, 10, 64); err == nil {
			refs = append(refs, revision)
		}
	}
	return refs
}
