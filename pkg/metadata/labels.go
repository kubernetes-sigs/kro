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
	"fmt"
	"maps"
	"strconv"
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
	// Removal timeline: TBD
	// LabelKROPrefix is the label key prefix used to identify KRO owned resources.
	LabelKROPrefix = v1alpha1.KRODomainName + "/"

	// InternalLabelKROPrefix is the label key prefix used to identify KRO owned resources.
	InternalLabelKROPrefix = "internal." + v1alpha1.KRODomainName + "/"
)

const (
	// Deprecated: v0.9.0
	// Use InternalNodeIDLabel instead.
	// Removal timeline: TBD
	NodeIDLabel = LabelKROPrefix + "node-id"

	// Deprecated: v0.9.0
	// Use InternalCollectionIndexLabel instead.
	// Removal timeline: TBD
	// Collection labels for tracking collection membership and position.
	// These enable querying collection resources and understanding their position.
	CollectionIndexLabel = LabelKROPrefix + "collection-index"
	// Deprecated: v0.9.0
	// Use InternalCollectionSizeLabel instead.
	// Removal timeline: TBD
	CollectionSizeLabel = LabelKROPrefix + "collection-size"

	// Deprecated: v0.9.0
	// Use InternalOwnedLabel instead.
	// Removal timeline: TBD
	OwnedLabel = LabelKROPrefix + "owned"
	// Deprecated: v0.9.0
	// Use InternalKROVersionLabel instead.
	// Removal timeline: TBD
	KROVersionLabel = LabelKROPrefix + "kro-version"

	ManagedByLabelKey = "app.kubernetes.io/managed-by"
	ManagedByKROValue = "kro"

	// Deprecated: v0.9.0
	// Use InternalInstanceIDLabel instead.
	// Removal timeline: TBD
	InstanceIDLabel = LabelKROPrefix + "instance-id"
	// Deprecated: v0.9.0
	// Use InternalInstanceLabel instead.
	// Removal timeline: TBD
	InstanceLabel = LabelKROPrefix + "instance-name"
	// Deprecated: v0.9.0
	// Use InternalInstanceNamespaceLabel instead.
	// Removal timeline: TBD
	InstanceNamespaceLabel = LabelKROPrefix + "instance-namespace"
	// Deprecated: v0.9.0
	// Use InternalInstanceGroupLabel instead.
	// Removal timeline: TBD
	InstanceGroupLabel = LabelKROPrefix + "instance-group"
	// Deprecated: v0.9.0
	// Use InternalInstanceVersionLabel instead.
	// Removal timeline: TBD
	InstanceVersionLabel = LabelKROPrefix + "instance-version"
	// Deprecated: v0.9.0
	// Use InternalInstanceKindLabel instead.
	// Removal timeline: TBD
	InstanceKindLabel = LabelKROPrefix + "instance-kind"

	// Deprecated: v0.9.0
	// Use InternalResourceGraphDefinitionIDLabel instead.
	// Removal timeline: TBD
	ResourceGraphDefinitionIDLabel = LabelKROPrefix + "resource-graph-definition-id"
	// Deprecated: v0.9.0
	// Use InternalResourceGraphDefinitionNameLabel instead.
	// Removal timeline: TBD
	ResourceGraphDefinitionNameLabel = LabelKROPrefix + "resource-graph-definition-name"
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

	InternalInstanceIDLabel        = InternalLabelKROPrefix + "instance-id"
	InternalInstanceLabel          = InternalLabelKROPrefix + "instance-name"
	InternalInstanceNamespaceLabel = InternalLabelKROPrefix + "instance-namespace"
	InternalInstanceGroupLabel     = InternalLabelKROPrefix + "instance-group"
	InternalInstanceVersionLabel   = InternalLabelKROPrefix + "instance-version"
	InternalInstanceKindLabel      = InternalLabelKROPrefix + "instance-kind"

	InternalResourceGraphDefinitionIDLabel   = InternalLabelKROPrefix + "resource-graph-definition-id"
	InternalResourceGraphDefinitionNameLabel = InternalLabelKROPrefix + "resource-graph-definition-name"
)

// IsKROOwned returns true if the resource is owned by KRO.
func IsKROOwned(meta metav1.Object) bool {
	labels := meta.GetLabels()
	if v, ok := labels[OwnedLabel]; ok {
		return booleanFromString(v)
	}
	return labels[ManagedByLabelKey] == ManagedByKROValue
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

	existingOwnerName := existing.Labels[ResourceGraphDefinitionNameLabel]
	existingOwnerID := existing.Labels[ResourceGraphDefinitionIDLabel]

	desiredOwnerName := desired.Labels[ResourceGraphDefinitionNameLabel]
	desiredOwnerID := desired.Labels[ResourceGraphDefinitionIDLabel]

	nameMatch = existingOwnerName == desiredOwnerName
	idMatch = existingOwnerID == desiredOwnerID

	return kroOwned, nameMatch, idMatch
}

var _ Labeler = GenericLabeler{}

// Labeler is an interface that defines a set of labels that can be
// applied to a resource.
type Labeler interface {
	Labels() map[string]string
	ApplyLabels(metav1.Object)
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
	labels := meta.GetLabels()
	if labels == nil {
		labels = make(map[string]string, len(gl))
	}
	maps.Copy(labels, gl)
	meta.SetLabels(labels)
}

// merge merges the labels from other into a copy of gl.
// Panics on duplicate keys — callers must ensure key sets are disjoint.
// When adding new label keys to any labeler constructor (MetaLabeler,
// InstanceLabeler, NodeLabeler), verify the key does not already appear
// in the other set being merged, or the controller will panic at runtime.
func (gl GenericLabeler) merge(other GenericLabeler) GenericLabeler {
	result := gl.Copy()
	for k, v := range other {
		if _, ok := result[k]; ok {
			panic(fmt.Sprintf("label key collision: %q exists in both labelers", k))
		}
		result[k] = v
	}
	return GenericLabeler(result)
}

// Copy returns a copy of the labels.
func (gl GenericLabeler) Copy() map[string]string {
	newGenericLabeler := map[string]string{}
	for k, v := range gl {
		newGenericLabeler[k] = v
	}
	return newGenericLabeler
}

// CollectionInfo holds collection item metadata for labeling.
type CollectionInfo struct {
	Index int
	Size  int
}

// NewKROMetaLabeler returns the KRO ownership labels common to all managed resources.
func NewKROMetaLabeler() GenericLabeler {
	v := safeVersion(version.GetVersionInfo().GitVersion)
	return GenericLabeler{
		OwnedLabel:              "true",
		KROVersionLabel:         v,
		InternalOwnedLabel:      "true",
		InternalKROVersionLabel: v,
	}
}

// NewInstanceLabeler returns the complete label set for an instance CR and its
// generated CRD. Includes KRO ownership, version, and RGD identity.
func NewInstanceLabeler(rgd *v1alpha1.ResourceGraphDefinition) GenericLabeler {
	return NewKROMetaLabeler().merge(GenericLabeler{
		// RGD identity
		ResourceGraphDefinitionIDLabel:   string(rgd.GetUID()),
		ResourceGraphDefinitionNameLabel: rgd.GetName(),
		// Internal RGD Identity labels
		InternalResourceGraphDefinitionIDLabel:   string(rgd.GetUID()),
		InternalResourceGraphDefinitionNameLabel: rgd.GetName(),
	})
}

// NewNodeLabeler returns the complete label set for a managed child resource (node).
// Includes KRO ownership, version, managed-by marker, instance identity, node
// identity, and optionally collection position.
func NewNodeLabeler(instance *unstructured.Unstructured, isInstanceNamespaced bool, nodeID string, collection *CollectionInfo) GenericLabeler {
	gvk := instance.GroupVersionKind()
	nodeLabels := GenericLabeler{
		// Managed-by marker
		ManagedByLabelKey: ManagedByKROValue,
		// Instance identity
		InstanceIDLabel:      string(instance.GetUID()),
		InstanceLabel:        instance.GetName(),
		InstanceGroupLabel:   gvk.Group,
		InstanceVersionLabel: gvk.Version,
		InstanceKindLabel:    gvk.Kind,
		// Internal instance identity
		InternalInstanceIDLabel:      string(instance.GetUID()),
		InternalInstanceLabel:        instance.GetName(),
		InternalInstanceGroupLabel:   gvk.Group,
		InternalInstanceVersionLabel: gvk.Version,
		InternalInstanceKindLabel:    gvk.Kind,
		// Node identity
		NodeIDLabel:         nodeID,
		InternalNodeIDLabel: nodeID,
	}
	// Collection position
	if collection != nil {
		nodeLabels[CollectionIndexLabel] = strconv.Itoa(collection.Index)
		nodeLabels[CollectionSizeLabel] = strconv.Itoa(collection.Size)
		nodeLabels[InternalCollectionIndexLabel] = strconv.Itoa(collection.Index)
		nodeLabels[InternalCollectionSizeLabel] = strconv.Itoa(collection.Size)
	}

	if isInstanceNamespaced {
		nodeLabels[InstanceNamespaceLabel] = instance.GetNamespace()
		nodeLabels[InternalInstanceNamespaceLabel] = instance.GetNamespace()
	}
	return NewKROMetaLabeler().merge(nodeLabels)
}

func safeVersion(version string) string {
	if validation.IsValidLabelValue(version) == nil {
		return version
	}
	// The script we use might add '+dirty' to development branches,
	// so let's try replacing '+' with '-'.
	return strings.ReplaceAll(version, "+", "-")
}

func booleanFromString(s string) bool {
	// for the sake of simplicity we'll avoid doing any kind
	// of parsing here. Since those labels are set by the controller
	// it self. We'll expect the same values back.
	return s == "true"
}
