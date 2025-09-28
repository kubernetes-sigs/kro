// Copyright 2023 The Kubernetes Authors.
// Copyright 2025 The Kube Resource Orchestrator Authors
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

// Applylib has code copied over from:
//   - kubectl pkg/cmd/apply/applyset.go
//   - kubebuilder-declarative-pattern/applylib
//   - Creating a simpler, self-contained version of the library that is purpose built for controllers.
//   - KEP describing applyset:
//     https://git.k8s.io/enhancements/keps/sig-cli/3659-kubectl-apply-prune#design-details-applyset-specification

package applyset

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/dynamic"
)

const (
	// This is set to an arbitrary number here for now.
	// This ensures we are no unbounded when pruning many GVKs.
	// Could be parameterized later on
	// TODO (barney-s): Possible parameterization target
	PruneGVKParallelizationLimit = 1
)

// PruneObject is an apiserver object that should be deleted as part of prune.
type PruneObject struct {
	*unstructured.Unstructured
	Mapping *meta.RESTMapping
}

// String returns a human-readable name of the object, for use in debug messages.
func (p *PruneObject) String() string {
	s := p.Mapping.GroupVersionKind.GroupKind().String()

	if p.GetNamespace() != "" {
		s += " " + p.GetNamespace() + "/" + p.GetName()
	} else {
		s += " " + p.GetName()
	}
	return s
}

// findAllObjectsToPrune returns the list of objects that will be pruned.
// Calling this instead of Prune can be useful for dry-run / diff behaviour.
func (a *applySet) findAllObjectsToPrune(
	ctx context.Context,
	dynamicClient dynamic.Interface,
	visitedUIDs sets.Set[types.UID],
) ([]PruneObject, error) {
	type task struct {
		namespace   string
		restMapping *meta.RESTMapping
		results     []PruneObject
	}

	tasks := []*task{}

	restMappings := map[schema.GroupKind]*meta.RESTMapping{}
	for gk, restMapping := range a.desiredRESTMappings {
		restMappings[gk] = restMapping
	}

	// add restmapping for older GKs
	for _, entry := range a.supersetGKs.UnsortedList() {
		if entry == "" {
			continue
		}
		gk := schema.ParseGroupKind(entry)
		if _, ok := restMappings[gk]; ok {
			continue
		}
		restMapping, err := a.restMapper.RESTMapping(gk)
		if err != nil {
			a.log.V(2).Info("no rest mapping for gk", "gk", gk)
			continue
		}
		restMappings[gk] = restMapping

	}
	// We run discovery in parallel, in as many goroutines as priority and fairness will allow
	// (We don't expect many requests in real-world scenarios - maybe tens, unlikely to be hundreds)
	for _, restMapping := range restMappings {
		switch restMapping.Scope.Name() {
		case meta.RESTScopeNameNamespace:
			for _, namespace := range a.supersetNamespaces.UnsortedList() {
				if namespace == "" {
					namespace = metav1.NamespaceDefault
				}
				tasks = append(tasks, &task{
					namespace:   namespace,
					restMapping: restMapping,
				})
			}

		case meta.RESTScopeNameRoot:
			tasks = append(tasks, &task{
				restMapping: restMapping,
			})

		default:
			return nil, fmt.Errorf("unhandled scope %q", restMapping.Scope.Name())
		}
	}

	if PruneGVKParallelizationLimit <= 1 {
		for i := range tasks {
			task := tasks[i]
			results, err := a.findObjectsToPrune(ctx, dynamicClient, visitedUIDs, task.namespace, task.restMapping)
			if err != nil {
				return nil, fmt.Errorf("listing %v objects for pruning: %w", task.restMapping.GroupVersionKind.String(), err)
			}
			task.results = results
		}
	} else {
		group, ctx := errgroup.WithContext(ctx)
		group.SetLimit(PruneGVKParallelizationLimit)
		for i := range tasks {
			task := tasks[i]
			group.Go(func() error {
				results, err := a.findObjectsToPrune(ctx, dynamicClient, visitedUIDs, task.namespace, task.restMapping)
				if err != nil {
					return fmt.Errorf("listing %v objects for pruning: %w", task.restMapping.GroupVersionKind.String(), err)
				}
				task.results = results
				return nil
			})
		}
		// Wait for all the goroutines to finish
		if err := group.Wait(); err != nil {
			return nil, err
		}
	}

	var allObjects []PruneObject
	for _, task := range tasks {
		allObjects = append(allObjects, task.results...)
	}
	return allObjects, nil
}

func (a *applySet) LabelSelectorForMembers() string {
	return metav1.FormatLabelSelector(&metav1.LabelSelector{
		MatchLabels: a.InjectApplysetLabels(nil),
	})
}

func (a *applySet) findObjectsToPrune(
	ctx context.Context,
	dynamicClient dynamic.Interface,
	visitedUIDs sets.Set[types.UID],
	namespace string,
	mapping *meta.RESTMapping,
) ([]PruneObject, error) {
	applysetLabelSelector := a.LabelSelectorForMembers()

	opt := metav1.ListOptions{
		LabelSelector: applysetLabelSelector,
	}

	a.log.V(2).Info("listing objects for pruning", "namespace", namespace, "resource", mapping.Resource)
	objects, err := dynamicClient.Resource(mapping.Resource).Namespace(namespace).List(ctx, opt)
	if err != nil {
		return nil, err
	}

	pruneObjects := []PruneObject{}
	for i := range objects.Items {
		obj := &objects.Items[i]

		uid := obj.GetUID()
		if visitedUIDs.Has(uid) {
			continue
		}

		pruneObjects = append(pruneObjects, PruneObject{
			Unstructured: obj,
			Mapping:      mapping,
		})

	}
	return pruneObjects, nil
}
