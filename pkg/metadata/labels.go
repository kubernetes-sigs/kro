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
	"maps"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/release-utils/version"
)

const (
	// LabelKROPrefix is retained for compatibility.
	// Deprecated: use KROPrefix.
	LabelKROPrefix = KROPrefix
)

const (
	NodeIDLabel = KROPrefix + "node-id"

	// Collection labels for tracking collection membership and position.
	// These enable querying collection resources and understanding their position.
	CollectionIndexLabel = KROPrefix + "collection-index"
	CollectionSizeLabel  = KROPrefix + "collection-size"

	OwnedLabel      = KROPrefix + "owned"
	KROVersionLabel = KROPrefix + "kro-version"

	ManagedByLabelKey = "app.kubernetes.io/managed-by"
	ManagedByKROValue = "kro"

	InstanceIDLabel        = KROPrefix + "instance-id"
	InstanceLabel          = KROPrefix + "instance-name"
	InstanceNamespaceLabel = KROPrefix + "instance-namespace"
	InstanceGroupLabel     = KROPrefix + "instance-group"
	InstanceVersionLabel   = KROPrefix + "instance-version"
	InstanceKindLabel      = KROPrefix + "instance-kind"

	ResourceGraphDefinitionIDLabel = KROPrefix + "resource-graph-definition-id"

	// ResourceGraphDefinitionNameLabel is only used when the name is short enough to fit in a label.
	// Prefer to use GetResourceGraphDefinitionName.
	ResourceGraphDefinitionNameLabel = KROPrefix + "resource-graph-definition-name"

	ResourceGraphDefinitionNamespaceLabel = KROPrefix + "resource-graph-definition-namespace"
	ResourceGraphDefinitionVersionLabel   = KROPrefix + "resource-graph-definition-version"
	// GraphRevisionHashLabel stores a label-safe representation of the GraphRevision spec hash.
	GraphRevisionHashLabel = KROPrefix + "graph-revision-hash"
)

// IsKROOwned returns true if the resource is owned by KRO.
func IsKROOwned(meta metav1.Object) bool {
	v, ok := meta.GetLabels()[OwnedLabel]
	if !ok {
		return meta.GetLabels()[ManagedByLabelKey] == ManagedByKROValue
	}
	return ok && booleanFromString(v)
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

	// Names are read annotation-first: RGD names longer than 63 characters are
	// only recorded in the annotation, never in the label.
	existingOwnerName, _ := GetResourceGraphDefinitionName(&existing)
	existingOwnerID := existing.Labels[ResourceGraphDefinitionIDLabel]

	desiredOwnerName, _ := GetResourceGraphDefinitionName(&desired)
	desiredOwnerID := desired.Labels[ResourceGraphDefinitionIDLabel]

	nameMatch = existingOwnerName == desiredOwnerName
	idMatch = existingOwnerID == desiredOwnerID

	return kroOwned, nameMatch, idMatch
}

var (
	ErrDuplicatedLabels = errors.New("duplicate labels")
)

var _ MetadataUpdater = GenericMetadataUpdater{}

// MetadataUpdater is an interface that defines a set of labels and annotations
// that can be applied to a resource.
type MetadataUpdater interface {
	GetLabels() map[string]string
	GetAnnotations() map[string]string
	Apply(metav1.Object)
	Merge(MetadataUpdater) (MetadataUpdater, error)
}

// GenericMetadataUpdater is a set of labels and annotations that are applied to a resource.
// It implements MetadataUpdater.
type GenericMetadataUpdater struct {
	Labels      map[string]string
	Annotations map[string]string
}

// Labels returns the labels.
func (gl GenericMetadataUpdater) GetLabels() map[string]string {
	return gl.Labels
}

// Annotations returns the annotations.
func (gl GenericMetadataUpdater) GetAnnotations() map[string]string {
	return gl.Annotations
}

// Apply applies the labels and annotations to the resource.
func (gl GenericMetadataUpdater) Apply(meta metav1.Object) {
	for k, v := range gl.Labels {
		setLabel(meta, k, v)
	}
	if len(gl.Annotations) != 0 {
		annotations := meta.GetAnnotations()
		if annotations == nil {
			annotations = maps.Clone(gl.Annotations)
		} else {
			maps.Copy(annotations, gl.Annotations)
		}
		meta.SetAnnotations(annotations)
	}
}

// Merge merges the labels and annotations from the other labeler into the current
// labeler. If there are any duplicate keys, an error is returned.
func (gl GenericMetadataUpdater) Merge(other MetadataUpdater) (MetadataUpdater, error) {
	newCopy := gl.Copy()
	if labels := other.GetLabels(); len(labels) != 0 {
		if newCopy.Labels == nil {
			newCopy.Labels = make(map[string]string, len(labels))
		}
		for k, v := range labels {
			if _, ok := newCopy.Labels[k]; ok {
				return nil, fmt.Errorf("%v: found key '%s' in both label maps", ErrDuplicatedLabels, k)
			}
			newCopy.Labels[k] = v
		}
	}
	if annotations := other.GetAnnotations(); len(annotations) != 0 {
		if newCopy.Annotations == nil {
			newCopy.Annotations = make(map[string]string, len(annotations))
		}
		for k, v := range annotations {
			if _, ok := newCopy.Annotations[k]; ok {
				return nil, fmt.Errorf("%v: found key '%s' in both annotation maps", ErrDuplicatedLabels, k)
			}
			newCopy.Annotations[k] = v
		}
	}
	return newCopy, nil
}

// Copy returns a copy of the labels and annotations.
func (gl GenericMetadataUpdater) Copy() GenericMetadataUpdater {
	var c GenericMetadataUpdater
	if len(gl.Labels) != 0 {
		c.Labels = maps.Clone(gl.Labels)
	}
	if len(gl.Annotations) != 0 {
		c.Annotations = maps.Clone(gl.Annotations)
	}
	return c
}

// NewResourceGraphDefinitionLabeler returns a new MetadataUpdater that sets the
// ResourceGraphDefinitionLabel and ResourceGraphDefinitionIDLabel labels on a resource,
// alongside the ResourceGraphDefinitionNameAnnotation annotation.
// The name label is omitted when the RGD name is not a valid label value
// (label values are limited to 63 characters, while RGD names can be up to
// 253); the full name is always recorded via the annotation instead.
func NewResourceGraphDefinitionLabeler(rgMeta metav1.Object) MetadataUpdater {
	name := rgMeta.GetName()
	labels := map[string]string{
		ResourceGraphDefinitionIDLabel: string(rgMeta.GetUID()),
	}
	if validation.IsValidLabelValue(name) == nil {
		labels[ResourceGraphDefinitionNameLabel] = name
	}
	return GenericMetadataUpdater{
		Labels: labels,
		Annotations: map[string]string{
			ResourceGraphDefinitionNameAnnotation: name,
		},
	}
}

// NewResourceGraphDefinitionNameLabeler returns a MetadataUpdater carrying only the
// RGD name.  The label is omitted when the RGD name is not a valid label
// value (label values are limited to 63 characters, while RGD names can be up to
// 253); the full name is always recorded via the annotation instead.
func NewResourceGraphDefinitionNameLabeler(name string) MetadataUpdater {
	updater := GenericMetadataUpdater{
		Annotations: map[string]string{
			ResourceGraphDefinitionNameAnnotation: name,
		},
	}
	if validation.IsValidLabelValue(name) == nil {
		updater.Labels = map[string]string{
			ResourceGraphDefinitionNameLabel: name,
		}
	}
	return updater
}

// NewGraphRevisionHashLabeler returns a new labeler that sets a label-safe
// representation of the GraphRevision spec hash on a resource.
func NewGraphRevisionHashLabeler(specHash string) MetadataUpdater {
	return GenericMetadataUpdater{
		Labels: map[string]string{
			GraphRevisionHashLabel: specHash,
		},
	}
}

// NewInstanceLabeler returns a new labeler that sets the InstanceLabel and
// InstanceIDLabel labels on a resource. The InstanceLabel is the namespace
// and name of the instance that was reconciled to create the resource.
// It also includes the instance's GVK to allow child
// resource handlers to filter events by parent instance type.
func NewInstanceLabeler(instance *unstructured.Unstructured, namespaced bool) MetadataUpdater {
	gvk := instance.GroupVersionKind()
	labels := map[string]string{
		InstanceIDLabel:      string(instance.GetUID()),
		InstanceLabel:        instance.GetName(),
		InstanceGroupLabel:   gvk.Group,
		InstanceVersionLabel: gvk.Version,
		InstanceKindLabel:    gvk.Kind,
	}
	if namespaced {
		labels[InstanceNamespaceLabel] = instance.GetNamespace()
	}
	return GenericMetadataUpdater{
		Labels: labels,
	}
}

// NewNodeLabeler returns a new labeler for child resources
// Only includes app.kubernetes.io/managed-by label, as other labels come from the parent labeler.
func NewNodeLabeler() MetadataUpdater {
	return GenericMetadataUpdater{
		Labels: map[string]string{
			ManagedByLabelKey: ManagedByKROValue,
		},
	}
}

// NewKROMetaLabeler returns a new labeler that sets the OwnedLabel, and
// KROVersion labels on a resource.
func NewKROMetaLabeler() MetadataUpdater {
	return GenericMetadataUpdater{
		Labels: map[string]string{
			OwnedLabel:      "true",
			KROVersionLabel: safeVersion(version.GetVersionInfo().GitVersion),
		},
	}
}

// NewCollectionItemLabeler returns a new labeler that sets collection-specific
// labels on a resource that is part of a collection (forEach expansion).
// - node-id: the resource ID from the RGD (e.g "workerPods")
// - collection-index: the position in the collection (e.g "0", "1", "2")
// - collection-size: the total number of items in the collection (e.g "3")
func NewCollectionItemLabeler(nodeID string, index, size int) MetadataUpdater {
	return GenericMetadataUpdater{
		Labels: map[string]string{
			NodeIDLabel:          nodeID,
			CollectionIndexLabel: strconv.Itoa(index),
			CollectionSizeLabel:  strconv.Itoa(size),
		},
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
