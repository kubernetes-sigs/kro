// Copyright 2025 The Kube Resource Orchestrator Authors
// Copyright 2022 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Derived from: https://github.com/kubernetes-sigs/kubebuilder-declarative-pattern/blob/master/applylib/applyset/results.go

package applyset

import (
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
)

// AppliedObject is a wrapper around an ApplyableObject that contains the last applied object
// It is used to track the applied object and any errors that occurred while applying it.
// It is also used to check if the object has been mutated in the cluster as part of the apply operation.
type AppliedObject struct {
	ApplyableObject
	LastApplied *unstructured.Unstructured
	Error       error
	Message     string
}

func (ao *AppliedObject) HasClusterMutation() bool {
	// If there was an error applying, we consider the object to have not changed.
	if ao.Error != nil {
		return false
	}

	// If the object was not applied, we consider it to have not changed.
	if ao.LastApplied == nil {
		return false
	}

	return ao.lastReadRevision != ao.LastApplied.GetResourceVersion()
}

type PrunedObject struct {
	PruneObject
	Error error
}

// ApplyResult summarizes the results of an apply operation
// It returns a list of applied and pruned objects
// AppliedObject has both the input object and the result of the apply operation
// Any errors returned by the apply call is also recorded in it.
// PrunedObject records any error returned by the delete call.
type ApplyResult struct {
	DesiredCount   int
	AppliedObjects []AppliedObject
	PrunedObjects  []PrunedObject
}

func (a *ApplyResult) Errors() error {
	return errors.Join(a.applyErrors(), a.pruneErrors())
}

func (a *ApplyResult) pruneErrors() error {
	var err error
	for _, pruned := range a.PrunedObjects {
		err = errors.Join(err, pruned.Error)
	}
	return err

}

func (a *ApplyResult) applyErrors() error {
	var err error
	if len(a.AppliedObjects) != a.DesiredCount {
		err = errors.Join(err, fmt.Errorf("expected %d applied objects, got %d",
			a.DesiredCount, len(a.AppliedObjects)))
	}
	for _, applied := range a.AppliedObjects {
		err = errors.Join(err, applied.Error)
	}
	return err
}

func (a *ApplyResult) AppliedUIDs() sets.Set[types.UID] {
	uids := sets.New[types.UID]()
	for _, applied := range a.AppliedObjects {
		if applied.Error != nil {
			continue
		}
		uids.Insert(applied.LastApplied.GetUID())
	}
	return uids
}

func (a *ApplyResult) HasClusterMutation() bool {
	for _, applied := range a.AppliedObjects {
		if applied.HasClusterMutation() {
			return true
		}
	}
	return false
}

func (a *ApplyResult) recordApplied(
	obj ApplyableObject,
	lastApplied *unstructured.Unstructured,
	err error,
) {
	ao := AppliedObject{
		ApplyableObject: obj,
		LastApplied:     lastApplied,
		Error:           err,
	}
	a.AppliedObjects = append(a.AppliedObjects, ao)
}

func (a *ApplyResult) recordPruned(
	obj PruneObject,
	err error,
) PrunedObject {
	po := PrunedObject{
		PruneObject: obj,
		Error:       err,
	}
	a.PrunedObjects = append(a.PrunedObjects, po)
	return po
}
