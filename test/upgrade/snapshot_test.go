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

package upgrade_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
)

const snapshotFileName = "upgrade-snapshot.json"

var graphRevisionGVR = schema.GroupVersionResource{
	Group:    "internal.kro.run",
	Version:  "v1alpha1",
	Resource: "graphrevisions",
}

// RGDSnapshot captures the state of a single RGD for comparison.
type RGDSnapshot struct {
	// Generation is the metadata.generation of the RGD.
	Generation int64 `json:"generation"`
	// ObservedGenerations maps condition type to its observedGeneration.
	ObservedGenerations map[string]int64 `json:"observedGenerations"`
}

// Snapshot captures pre-upgrade state for post-upgrade comparison.
type Snapshot struct {
	// RGDs maps RGD name to its pre-upgrade state.
	RGDs map[string]RGDSnapshot `json:"rgds"`
	// GRCountPerRGD maps RGD name to its GraphRevision count.
	GRCountPerRGD map[string]int `json:"grCountPerRGD"`
	// GRHashPerRGD maps RGD name to the spec hash from its latest GraphRevision.
	GRHashPerRGD map[string]string `json:"grHashPerRGD"`
	// TotalGRCount is the total number of GraphRevisions across all RGDs.
	TotalGRCount int `json:"totalGRCount"`
	// HasGRSupport indicates whether GraphRevisions existed pre-upgrade.
	HasGRSupport bool `json:"hasGRSupport"`
}

var _ = ginkgo.Describe("Snapshot", ginkgo.Ordered, func() {
	ginkgo.When("running in pre-upgrade mode", func() {
		ginkgo.BeforeAll(func() {
			if !isPreUpgrade() {
				ginkgo.Skip("Snapshot recording only runs in pre-upgrade mode")
			}
		})

		ginkgo.It("should record pre-upgrade snapshot", func() {
			snapshot := Snapshot{
				RGDs:          make(map[string]RGDSnapshot),
				GRCountPerRGD: make(map[string]int),
				GRHashPerRGD:  make(map[string]string),
			}

			// Record RGD generations and condition observedGenerations
			rgdList := &krov1alpha1.ResourceGraphDefinitionList{}
			gomega.Expect(k8sClient.List(ctx, rgdList)).To(gomega.Succeed())

			for _, rgd := range rgdList.Items {
				rgdSnap := RGDSnapshot{
					Generation:          rgd.Generation,
					ObservedGenerations: make(map[string]int64),
				}
				for _, cond := range rgd.Status.Conditions {
					rgdSnap.ObservedGenerations[string(cond.Type)] = cond.ObservedGeneration
				}
				snapshot.RGDs[rgd.Name] = rgdSnap
			}

			// Check if GraphRevision CRD exists
			grList, err := dynamicClient.Resource(graphRevisionGVR).List(ctx, metav1.ListOptions{})
			if err != nil {
				ginkgo.GinkgoLogr.Info("GraphRevision CRD not found, pre-GR version", "error", err)
				snapshot.HasGRSupport = false
			} else {
				snapshot.HasGRSupport = true
				snapshot.TotalGRCount = len(grList.Items)

				// Index GRs by owner RGD
				for _, gr := range grList.Items {
					rgdName := getRGDNameFromGR(gr)
					if rgdName == "" {
						continue
					}
					snapshot.GRCountPerRGD[rgdName]++

					// Record the spec hash label
					labels := gr.GetLabels()
					if hash, ok := labels["internal.kro.run/spec-hash"]; ok {
						// Keep the latest (highest revision) hash per RGD
						snapshot.GRHashPerRGD[rgdName] = hash
					}
				}
			}

			// Write snapshot to artifacts
			snapshotPath := filepath.Join(artifactsDir, snapshotFileName)
			gomega.Expect(os.MkdirAll(artifactsDir, 0o755)).To(gomega.Succeed())

			data, err := json.MarshalIndent(snapshot, "", "  ")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(os.WriteFile(snapshotPath, data, 0o644)).To(gomega.Succeed())

			ginkgo.GinkgoLogr.Info("Snapshot recorded",
				"path", snapshotPath,
				"rgdCount", len(snapshot.RGDs),
				"hasGRSupport", snapshot.HasGRSupport,
				"totalGRCount", snapshot.TotalGRCount,
			)
		})
	})
})

// loadSnapshot reads the snapshot written during pre-upgrade.
func loadSnapshot() (*Snapshot, error) {
	snapshotPath := filepath.Join(artifactsDir, snapshotFileName)
	data, err := os.ReadFile(snapshotPath)
	if err != nil {
		return nil, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

// getRGDNameFromGR extracts the owning RGD name from a GraphRevision's owner references.
func getRGDNameFromGR(gr unstructured.Unstructured) string {
	owners := gr.GetOwnerReferences()
	for _, owner := range owners {
		if owner.Kind == "ResourceGraphDefinition" {
			return owner.Name
		}
	}
	// Fallback: check labels
	labels := gr.GetLabels()
	if name, ok := labels["internal.kro.run/rgd-name"]; ok {
		return name
	}
	return ""
}

// captureGRCounts waits for GraphRevisions to exist for all RGDs, then
// returns the count per RGD. Called after waitForControllerReady guarantees
// the GR CRD is Established.
func captureGRCounts() map[string]int {
	var counts map[string]int

	// First, get the list of all RGDs so we can wait for a GR per RGD.
	rgdList := &krov1alpha1.ResourceGraphDefinitionList{}
	gomega.Expect(k8sClient.List(ctx, rgdList)).To(gomega.Succeed())

	gomega.Eventually(func(g gomega.Gomega) {
		counts = make(map[string]int)
		grList, err := dynamicClient.Resource(graphRevisionGVR).List(ctx, metav1.ListOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())

		for _, gr := range grList.Items {
			rgdName := getRGDNameFromGR(gr)
			if rgdName != "" {
				counts[rgdName]++
			}
		}
		// Wait until every RGD has at least one GraphRevision, not just "any GR exists".
		for _, rgd := range rgdList.Items {
			g.Expect(counts).To(gomega.HaveKey(rgd.Name),
				"RGD %s should have at least one GraphRevision", rgd.Name)
		}
	}, 3*time.Minute, 3*time.Second).Should(gomega.Succeed())

	ginkgo.GinkgoLogr.Info("Captured initial GR counts", "counts", counts)
	return counts
}

// Ensure krov1alpha1 import is used
var _ krov1alpha1.ConditionType
