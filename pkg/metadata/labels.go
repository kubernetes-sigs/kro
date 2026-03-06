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

package metadata

import (
	"errors"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/release-utils/version"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

const (
	// Deprecated: v0.9.0
	// Use InternalLabelKROPrefix instead.
	// This label will be removed in v0.10.0
	// LabelKROPrefix is the label key prefix used to identify KRO owned resources.
	LabelKROPrefix = v1alpha1.KRODomainName + "/"

	// InternalLabelKROPrefix is the label key prefix used to identify KRO owned resources.
	InternalLabelKROPrefix = "internal." + v1alpha1.KRODomainName + "/"
)

const (
	// Deprecated: v0.9.0
	// Use InternalNodeIDLabel instead.
	// This label will be removed in v0.10.0
	NodeIDLabel = LabelKROPrefix + "node-id"

	// Deprecated: v0.9.0
	// Use InternalCollectionIndexLabel instead.
	// This label will be removed in v0.10.0
	// Collection labels for tracking collection membership and position.
	// These enable querying collection resources and understanding their position.
	CollectionIndexLabel = LabelKROPrefix + "collection-index"
	// Deprecated: v0.9.0
	// Use InternalCollectionSizeLabel instead.
	// This label will be removed in v0.10.0
	CollectionSizeLabel = LabelKROPrefix + "collection-size"

	// Deprecated: v0.9.0
	// Use InternalOwnedLabel instead.
	// This label will be removed in v0.10.0
	OwnedLabel = LabelKROPrefix + "owned"
	// Deprecated: v0.9.0
	// Use InternalKROVersionLabel instead.
	// This label will be removed in v0.10.0
	KROVersionLabel = LabelKROPrefix + "kro-version"

	// Deprecated: v0.9.0
	// Use InternalManagedByLabelKey instead.
	// This label will be removed in v0.10.0
	ManagedByLabelKey = "app.kubernetes.io/managed-by"
	// Deprecated: v0.9.0
	// Use InternalManagedByKROValue instead.
	// This label will be removed in v0.10.0
	ManagedByKROValue = "kro"

	// Deprecated: v0.9.0
	// Use InternalInstanceIDLabel instead.
	// This label will be removed in v0.10.0
	InstanceIDLabel = LabelKROPrefix + "instance-id"
	// Deprecated: v0.9.0
	// Use InternalInstanceLabel instead.
	// This label will be removed in v0.10.0
	InstanceLabel = LabelKROPrefix + "instance-name"
	// Deprecated: v0.9.0
	// Use InternalInstanceNamespaceLabel instead.
	// This label will be removed in v0.10.0
	InstanceNamespaceLabel = LabelKROPrefix + "instance-namespace"
	// Deprecated: v0.9.0
	// Use InternalInstanceGroupLabel instead.
	// This label will be removed in v0.10.0
	InstanceGroupLabel = LabelKROPrefix + "instance-group"
	// Deprecated: v0.9.0
	// Use InternalInstanceVersionLabel instead.
	// This label will be removed in v0.10.0
	InstanceVersionLabel = LabelKROPrefix + "instance-version"
	// Deprecated: v0.9.0
	// Use InternalInstanceKindLabel instead.
	// This label will be removed in v0.10.0
	InstanceKindLabel = LabelKROPrefix + "instance-kind"
	// Deprecated: v0.9.0
	// Use InternalInstanceReconcileLabel instead.
	// This label will be removed in v0.10.0
	InstanceReconcileLabel = LabelKROPrefix + "reconcile"

	// Deprecated: v0.9.0
	// Use InternalResourceGraphDefinitionIDLabel instead.
	// This label will be removed in v0.10.0
	ResourceGraphDefinitionIDLabel = LabelKROPrefix + "resource-graph-definition-id"
	// Deprecated: v0.9.0
	// Use InternalResourceGraphDefinitionNameLabel instead.
	// This label will be removed in v0.10.0
	ResourceGraphDefinitionNameLabel = LabelKROPrefix + "resource-graph-definition-name"
	// Deprecated: v0.9.0
	// Use InternalResourceGraphDefinitionNamespaceLabel instead.
	// This label will be removed in v0.10.0
	ResourceGraphDefinitionNamespaceLabel = LabelKROPrefix + "resource-graph-definition-namespace"
	// Deprecated: v0.9.0
	// Use InternalResourceGraphDefinitionVersionLabel instead.
	// This label will be removed in v0.10.0
	ResourceGraphDefinitionVersionLabel = LabelKROPrefix + "resource-graph-definition-version"
)

// Internal labels
const (
	InternalNodeIDLabel = InternalLabelKROPrefix + "node-id"

	// Collection labels for tracking collection membership and position.
	// These enable querying collection resources and understanding their position.
	InternalCollectionIndexLabel = InternalLabelKROPrefix + "collection-index"
	InternalCollectionSizeLabel  = InternalLabelKROPrefix + "collection-size"

	InternalOwnedLabel      = InternalLabelKROPrefix + "owned"
	InternalKROVersionLabel = InternalLabelKROPrefix + "kro-version"

	InternalManagedByLabelKey = "app.kubernetes.io/managed-by"
	InternalManagedByKROValue = "kro"

	InternalInstanceIDLabel        = InternalLabelKROPrefix + "instance-id"
	InternalInstanceLabel          = InternalLabelKROPrefix + "instance-name"
	InternalInstanceNamespaceLabel = InternalLabelKROPrefix + "instance-namespace"
	InternalInstanceGroupLabel     = InternalLabelKROPrefix + "instance-group"
	InternalInstanceVersionLabel   = InternalLabelKROPrefix + "instance-version"
	InternalInstanceKindLabel      = InternalLabelKROPrefix + "instance-kind"
	InternalInstanceReconcileLabel = InternalLabelKROPrefix + "reconcile"

	InternalResourceGraphDefinitionIDLabel        = InternalLabelKROPrefix + "resource-graph-definition-id"
	InternalResourceGraphDefinitionNameLabel      = InternalLabelKROPrefix + "resource-graph-definition-name"
	InternalResourceGraphDefinitionNamespaceLabel = InternalLabelKROPrefix + "resource-graph-definition-namespace"
	InternalResourceGraphDefinitionVersionLabel   = InternalLabelKROPrefix + "resource-graph-definition-version"
)

// IsKROOwned returns true if the resource is owned by KRO.
// Checks Internal label first, falling back to the deprecated label.
func IsKROOwned(meta metav1.Object) bool {
	labels := meta.GetLabels()
	v, ok := LabelWithFallback(labels, InternalOwnedLabel, OwnedLabel)
	if !ok {
		return labels[ManagedByLabelKey] == ManagedByKROValue
	}
	return booleanFromString(v)
}

// CompareRGDOwnership compares RGD ownership labels between two resources.
// Returns three booleans:
//   - kroOwned: whether the existing resource is owned by KRO
//   - nameMatch: whether both resources have the same RGD name
//   - idMatch: whether both resources have the same RGD ID
//
// This allows callers to distinguish between different ownership scenarios:
//   - kroOwned=true, nameMatch=true, idMatch=true: same RGD, normal update
//   - kroOwned=true, nameMatch=true, idMatch=false: same RGD name, different ID (adoption)
//   - kroOwned=true, nameMatch=false: different RGD (conflict)
//   - kroOwned=false: not owned by KRO (conflict)
func CompareRGDOwnership(existing, desired metav1.ObjectMeta) (kroOwned, nameMatch, idMatch bool) {
	kroOwned = IsKROOwned(&existing)
	if !kroOwned {
		return false, false, false
	}

	existingOwnerName, _ := LabelWithFallback(existing.Labels, InternalResourceGraphDefinitionNameLabel, ResourceGraphDefinitionNameLabel)
	existingOwnerID, _ := LabelWithFallback(existing.Labels, InternalResourceGraphDefinitionIDLabel, ResourceGraphDefinitionIDLabel)

	desiredOwnerName, _ := LabelWithFallback(desired.Labels, InternalResourceGraphDefinitionNameLabel, ResourceGraphDefinitionNameLabel)
	desiredOwnerID, _ := LabelWithFallback(desired.Labels, InternalResourceGraphDefinitionIDLabel, ResourceGraphDefinitionIDLabel)

	nameMatch = existingOwnerName == desiredOwnerName
	idMatch = existingOwnerID == desiredOwnerID

	return kroOwned, nameMatch, idMatch
}

var (
	ErrDuplicatedLabels = errors.New("duplicate labels")
)

var _ Labeler = GenericLabeler{}

// Labeler is an interface that defines a set of labels that can be
// applied to a resource.
type Labeler interface {
	Labels() map[string]string
	ApplyLabels(metav1.Object)
	Merge(Labeler) (Labeler, error)
}

// GenericLabeler is a map of labels that can be applied to a resource.
// It implements the Labeler interface.
type GenericLabeler map[string]string

// Labels returns the labels.
func (gl GenericLabeler) Labels() map[string]string {
	return gl
}

// ApplyLabels applies the labels to the resource.
func (gl GenericLabeler) ApplyLabels(meta metav1.Object) {
	for k, v := range gl {
		setLabel(meta, k, v)
	}
}

// Merge merges the labels from the other labeler into the current
// labeler. If there are any duplicate keys, an error is returned.
func (gl GenericLabeler) Merge(other Labeler) (Labeler, error) {
	newLabels := gl.Copy()
	for k, v := range other.Labels() {
		if _, ok := newLabels[k]; ok {
			return nil, fmt.Errorf("%v: found key '%s' in both maps", ErrDuplicatedLabels, k)
		}
		newLabels[k] = v
	}
	return GenericLabeler(newLabels), nil
}

// Copy returns a copy of the labels.
func (gl GenericLabeler) Copy() map[string]string {
	newGenericLabeler := map[string]string{}
	for k, v := range gl {
		newGenericLabeler[k] = v
	}
	return newGenericLabeler
}

// NewResourceGraphDefinitionLabeler returns a new labeler that sets the
// ResourceGraphDefinitionLabel and ResourceGraphDefinitionIDLabel labels on a resource.
func NewResourceGraphDefinitionLabeler(rgMeta metav1.Object) GenericLabeler {
	return map[string]string{
		ResourceGraphDefinitionIDLabel:           string(rgMeta.GetUID()),
		ResourceGraphDefinitionNameLabel:         rgMeta.GetName(),
		InternalResourceGraphDefinitionIDLabel:   string(rgMeta.GetUID()),
		InternalResourceGraphDefinitionNameLabel: rgMeta.GetName(),
	}
}

// NewInstanceLabeler returns a new labeler that sets the InstanceLabel and
// InstanceIDLabel labels on a resource. The InstanceLabel is the namespace
// and name of the instance that was reconciled to create the resource.
// It also includes the instance's GVK to allow child
// resource handlers to filter events by parent instance type.
func NewInstanceLabeler(instance *unstructured.Unstructured) GenericLabeler {
	gvk := instance.GroupVersionKind()
	return map[string]string{
		InstanceIDLabel:        string(instance.GetUID()),
		InstanceLabel:          instance.GetName(),
		InstanceNamespaceLabel: instance.GetNamespace(),
		InstanceGroupLabel:     gvk.Group,
		InstanceVersionLabel:   gvk.Version,
		InstanceKindLabel:      gvk.Kind,

		InternalInstanceIDLabel:        string(instance.GetUID()),
		InternalInstanceLabel:          instance.GetName(),
		InternalInstanceNamespaceLabel: instance.GetNamespace(),
		InternalInstanceGroupLabel:     gvk.Group,
		InternalInstanceVersionLabel:   gvk.Version,
		InternalInstanceKindLabel:      gvk.Kind,
	}
}

// NewNodeLabeler returns a new labeler for child resources
// Only includes app.kubernetes.io/managed-by label, as other labels come from the parent labeler.
func NewNodeLabeler() GenericLabeler {
	return map[string]string{
		ManagedByLabelKey: ManagedByKROValue,
	}
}

// NewKROMetaLabeler returns a new labeler that sets the OwnedLabel, and
// KROVersion labels on a resource.
func NewKROMetaLabeler() GenericLabeler {
	v := safeVersion(version.GetVersionInfo().GitVersion)
	return map[string]string{
		OwnedLabel:      "true",
		KROVersionLabel: v,

		InternalOwnedLabel:      "true",
		InternalKROVersionLabel: v,
	}
}

func safeVersion(version string) string {
	if validation.IsValidLabelValue(version) == nil {
		return version
	}
	// The script we use might add '+dirty' to development branches,
	// so let's try replacing '+' with '-'.
	return strings.ReplaceAll(version, "+", "-")
}

// LabelWithFallback looks up a label by its Internal key first, falling back
// to the deprecated key. This makes reads resilient during the migration period
// where resources may carry only old labels (pre-migration) or both (post-migration).
func LabelWithFallback(labels map[string]string, internalKey, deprecatedKey string) (string, bool) {
	if v, ok := labels[internalKey]; ok {
		return v, true
	}
	v, ok := labels[deprecatedKey]
	return v, ok
}

func booleanFromString(s string) bool {
	// for the sake of simplicity we'll avoid doing any kind
	// of parsing here. Since those labels are set by the controller
	// it self. We'll expect the same values back.
	return s == "true"
}

// Helper function to set a label
func setLabel(meta metav1.Object, key, value string) {
	labels := meta.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[key] = value
	meta.SetLabels(labels)
}
