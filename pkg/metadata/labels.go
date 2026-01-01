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
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/release-utils/version"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

const (
	// LabelKROPrefix is the label key prefix used to identify KRO owned resources.
	LabelKROPrefix = v1alpha1.KRODomainName + "/"
)

const (
	NodeIDLabel = LabelKROPrefix + "node-id"

	// Collection labels for tracking collection membership and position.
	// These enable querying collection resources and understanding their position.
	CollectionIndexLabel = LabelKROPrefix + "collection-index"
	CollectionSizeLabel  = LabelKROPrefix + "collection-size"

	OwnedLabel      = LabelKROPrefix + "owned"
	KROVersionLabel = LabelKROPrefix + "kro-version"

	ManagedByLabelKey = "app.kubernetes.io/managed-by"
	ManagedByKROValue = "kro"

	InstanceIDLabel        = LabelKROPrefix + "instance-id"
	InstanceLabel          = LabelKROPrefix + "instance-name"
	InstanceNamespaceLabel = LabelKROPrefix + "instance-namespace"

	ResourceGraphDefinitionIDLabel        = LabelKROPrefix + "resource-graph-definition-id"
	ResourceGraphDefinitionNameLabel      = LabelKROPrefix + "resource-graph-definition-name"
	ResourceGraphDefinitionNamespaceLabel = LabelKROPrefix + "resource-graph-definition-namespace"
	ResourceGraphDefinitionVersionLabel   = LabelKROPrefix + "resource-graph-definition-version"
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

	existingOwnerName := existing.Labels[ResourceGraphDefinitionNameLabel]
	existingOwnerID := existing.Labels[ResourceGraphDefinitionIDLabel]

	desiredOwnerName := desired.Labels[ResourceGraphDefinitionNameLabel]
	desiredOwnerID := desired.Labels[ResourceGraphDefinitionIDLabel]

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
		ResourceGraphDefinitionIDLabel:   string(rgMeta.GetUID()),
		ResourceGraphDefinitionNameLabel: rgMeta.GetName(),
	}
}

// NewInstanceLabeler returns a new labeler that sets the InstanceLabel and
// InstanceIDLabel labels on a resource. The InstanceLabel is the namespace
// and name of the instance that was reconciled to create the resource.
func NewInstanceLabeler(instanceMeta metav1.Object) GenericLabeler {
	return map[string]string{
		InstanceIDLabel:        string(instanceMeta.GetUID()),
		InstanceLabel:          instanceMeta.GetName(),
		InstanceNamespaceLabel: instanceMeta.GetNamespace(),
	}
}

// NewKROMetaLabeler returns a new labeler that sets the OwnedLabel,
// KROVersion, and ManagedBy labels on a resource.
func NewKROMetaLabeler() GenericLabeler {
	return map[string]string{
		OwnedLabel:        "true",
		KROVersionLabel:   safeVersion(version.GetVersionInfo().GitVersion),
		ManagedByLabelKey: ManagedByKROValue,
	}
}

// NewCollectionItemLabeler returns a new labeler that sets collection-specific
// labels on a resource that is part of a collection (forEach expansion).
// - node-id: the resource ID from the RGD (e.g "workerPods")
// - collection-index: the position in the collection (e.g "0", "1", "2")
// - collection-size: the total number of items in the collection (e.g "3")
func NewCollectionItemLabeler(nodeID string, index, size int) GenericLabeler {
	return map[string]string{
		NodeIDLabel:          nodeID,
		CollectionIndexLabel: strconv.Itoa(index),
		CollectionSizeLabel:  strconv.Itoa(size),
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
