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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

func TestResourceGraphDefinitionPrimaryPredicate(t *testing.T) {
	t.Parallel()

	pred := resourceGraphDefinitionPrimaryPredicate()
	deletionTime := metav1.NewTime(time.Unix(123, 0))
	testCases := []struct {
		name string
		run  func(predicate.Predicate) bool
		want bool
	}{
		{
			name: "accepts generation changes",
			run: func(pred predicate.Predicate) bool {
				return pred.Update(event.UpdateEvent{
					ObjectOld: newPredicateTestRGD(1, nil),
					ObjectNew: newPredicateTestRGD(2, nil),
				})
			},
			want: true,
		},
		{
			name: "accepts deletion timestamp transitions",
			run: func(pred predicate.Predicate) bool {
				return pred.Update(event.UpdateEvent{
					ObjectOld: newPredicateTestRGD(1, nil),
					ObjectNew: newPredicateTestRGD(1, &deletionTime),
				})
			},
			want: true,
		},
		{
			name: "ignores status only updates",
			run: func(pred predicate.Predicate) bool {
				return pred.Update(event.UpdateEvent{
					ObjectOld: newPredicateTestRGD(1, nil),
					ObjectNew: newPredicateTestRGD(1, nil),
				})
			},
			want: false,
		},
		{
			name: "keeps create events enabled",
			run: func(pred predicate.Predicate) bool {
				return pred.Create(event.CreateEvent{Object: newPredicateTestRGD(1, nil)})
			},
			want: true,
		},
		{
			name: "keeps delete events enabled",
			run: func(pred predicate.Predicate) bool {
				return pred.Delete(event.DeleteEvent{Object: newPredicateTestRGD(1, nil)})
			},
			want: true,
		},
	}

	for _, tc := range testCases {
		if got := tc.run(pred); got != tc.want {
			t.Errorf("%s: got %t, want %t", tc.name, got, tc.want)
		}
	}
}

func newPredicateTestRGD(generation int64, deletionTimestamp *metav1.Time) *v1alpha1.ResourceGraphDefinition {
	return &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-rgd",
			Generation:        generation,
			DeletionTimestamp: deletionTimestamp,
		},
	}
}
