// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package metadata

import (
	"errors"
	"fmt"

	"github.com/awslabs/symphony/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// LabelSymphony is the label key used to identify Symphony owned resources.
	LabelSymphonyPrefix = v1alpha1.SymphonyDomainName + "/"
)

const (
	NodeIDLabel = LabelSymphonyPrefix + "node-id"

	OwnedLabel           = LabelSymphonyPrefix + "owned"
	SymphonyVersionLabel = LabelSymphonyPrefix + "symphony-version"
	ControllerPodIDLabel = LabelSymphonyPrefix + "controller-pod-id"

	InstanceIDLabel        = LabelSymphonyPrefix + "instance-id"
	InstanceLabel          = LabelSymphonyPrefix + "instance-name"
	InstanceNamespaceLabel = LabelSymphonyPrefix + "instance-namespace"

	ResourceGroupIDLabel        = LabelSymphonyPrefix + "resource-group-id"
	ResourceGroupNameLabel      = LabelSymphonyPrefix + "resource-group-name"
	ResourceGroupNamespaceLabel = LabelSymphonyPrefix + "resource-group-namespace"
	ResourceGroupVersionLabel   = LabelSymphonyPrefix + "resource-group-version"
)

// IsSymphonyOwned returns true if the resource is owned by Symphony.
func IsSymphonyOwned(meta metav1.ObjectMeta) bool {
	v, ok := meta.Labels[OwnedLabel]
	return ok && booleanFromString(v)
}

// SetSymphonyOwned sets the OwnedLabel to true on the resource.
func SetSymphonyOwned(meta metav1.ObjectMeta) {
	setLabel(&meta, OwnedLabel, stringFromBoolean(true))
}

// SetSymphonyUnowned sets the OwnedLabel to false on the resource.
func SetSymphonyUnowned(meta metav1.ObjectMeta) {
	setLabel(&meta, OwnedLabel, stringFromBoolean(false))
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

// NewResourceGroupLabeler returns a new labeler that sets the
// ResourceGroupLabel and ResourceGroupIDLabel labels on a resource.
func NewResourceGroupLabeler(rgMeta metav1.Object) GenericLabeler {
	return map[string]string{
		ResourceGroupIDLabel:        string(rgMeta.GetUID()),
		ResourceGroupNameLabel:      rgMeta.GetName(),
		ResourceGroupNamespaceLabel: rgMeta.GetNamespace(),
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

// NewSymphonyMetaLabeler returns a new labeler that sets the OwnedLabel,
// SymphonyVersion, and ControllerPodID labels on a resource.
func NewSymphonyMetaLabeler(
	symphonyVersion string,
	controllerPodID string,
) GenericLabeler {
	return map[string]string{
		OwnedLabel:           "true",
		SymphonyVersionLabel: symphonyVersion,
		ControllerPodIDLabel: controllerPodID,
	}
}

func booleanFromString(s string) bool {
	// for the sake of simplicy we'll avoid doing any kind
	// of parsing here. Since those labels are set by the controller
	// it self. We'll expect the same values back.
	return s == "true"
}

func stringFromBoolean(b bool) string {
	if b {
		return "true"
	}
	return "false"
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
