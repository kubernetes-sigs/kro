// Copyright 2025 The Kube Resource Orchestrator Authors
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

package instance

import (
	"fmt"
	"strconv"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// instanceRevisions is the pair of revision numbers kro writes onto an
// instance's status: the revision it reconciled against and the revision it is
// meant to reconcile against.
type instanceRevisions struct {
	Current int64
	Target  int64
}

// resolveTargetRevision decides which graph revision an instance must
// reconcile against.
//
// annotation is the raw kro.run/graph-revision value, strategy the RGD's
// effective rollout strategy, and current the revision already recorded in
// status.graphRevision (0 when the instance never reconciled).
//
// followLatest reports that the caller must resolve the latest issued
// revision; target is only meaningful when followLatest is false.
func resolveTargetRevision(annotation string, strategy v1alpha1.RolloutStrategy, current int64) (target int64, followLatest bool, err error) {
	switch annotation {
	case "":
		// No pin. Manual holds the instance on the revision it already
		// reconciled against; a fresh instance has none, so it adopts the
		// latest issued revision and stays there.
		if strategy == v1alpha1.RolloutStrategyManual && current > 0 {
			return current, false, nil
		}
		return 0, true, nil
	case v1alpha1.GraphRevisionLatest:
		return 0, true, nil
	}

	pinned, parseErr := strconv.ParseInt(annotation, 10, 64)
	if parseErr != nil || pinned < 1 {
		return 0, false, fmt.Errorf(
			"invalid %s annotation value %q: must be a revision number or %q",
			v1alpha1.InstanceGraphRevisionAnnotation, annotation, v1alpha1.GraphRevisionLatest,
		)
	}
	return pinned, false, nil
}

// currentGraphRevision reads status.graphRevision off an instance as fetched,
// before any marker mutates it. 0 means the instance never recorded one.
func currentGraphRevision(inst *unstructured.Unstructured) int64 {
	status, ok := inst.Object["status"].(map[string]any)
	if !ok {
		return 0
	}
	switch rev := status["graphRevision"].(type) {
	case int64:
		return rev
	case float64:
		return int64(rev)
	case int:
		return int64(rev)
	default:
		return 0
	}
}
