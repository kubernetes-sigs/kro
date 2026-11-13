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

package versioning_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime"

	ctrlinstance "github.com/kubernetes-sigs/kro/pkg/controller/instance"
	"github.com/kubernetes-sigs/kro/pkg/features"
	"github.com/kubernetes-sigs/kro/pkg/testutil/environment"
)

var env *environment.Environment

// maxGraphRevisions is deliberately small so retention GC runs after a couple
// of RGD edits instead of needing dozens.
const maxGraphRevisions = 2

// TestVersioning runs the KREP-009 suite with both versioning gates enabled.
// It is a separate suite from core so the core specs keep exercising the
// gates-off behavior.
func TestVersioning(t *testing.T) {
	if err := features.FeatureGate.Set("GraphRevisionPinning=true"); err != nil {
		t.Fatalf("failed to enable GraphRevisionPinning feature gate: %v", err)
	}
	if err := features.FeatureGate.Set("MultiVersionSchema=true"); err != nil {
		t.Fatalf("failed to enable MultiVersionSchema feature gate: %v", err)
	}

	RegisterFailHandler(Fail)

	SynchronizedBeforeSuite(func() []byte {
		var err error
		env, err = environment.New(t.Context(),
			environment.ControllerConfig{
				AllowCRDDeletion:  true,
				MaxGraphRevisions: maxGraphRevisions,
				ReconcileConfig: ctrlinstance.ReconcileConfig{
					DefaultRequeueDuration: 5 * time.Second,
				},
				LogWriter: GinkgoWriter,
			},
		)
		Expect(err).NotTo(HaveOccurred())

		data, err := environment.EncodeRESTConfig(env.ClientSet.RESTConfig())
		Expect(err).NotTo(HaveOccurred())
		return data
	}, func(data []byte) {
		if env == nil {
			cfg, err := environment.DecodeRESTConfig(data)
			Expect(err).NotTo(HaveOccurred())
			env, err = environment.NewShared(t.Context(), cfg)
			Expect(err).NotTo(HaveOccurred())
		}
		// Each parallel process gets its own virtual kro.run API group so
		// specs sharing a schema kind never collide on the shared apiserver's
		// cluster-scoped CRDs.
		env.Client = environment.NewGroupIsolatingClient(env.Client,
			fmt.Sprintf("p%d", GinkgoParallelProcess()))
	})

	SynchronizedAfterSuite(func() {
		if GinkgoParallelProcess() != 1 {
			Expect(env.Stop()).To(Succeed())
		}
	}, func() {
		err := (func() (err error) {
			sleepTime := 1 * time.Millisecond
			for range 12 { // Exponentially sleep up to ~4s
				if err = env.Stop(); err == nil {
					return
				}
				sleepTime *= 2
				time.Sleep(sleepTime)
			}
			return
		})()
		Expect(err).NotTo(HaveOccurred())
	})

	RunSpecs(t, "Versioning Suite")
}

// toRawExtension converts a map into the RawExtension shape RGD schemas use.
func toRawExtension(v any) runtime.RawExtension {
	rawJSON, err := json.Marshal(v)
	Expect(err).NotTo(HaveOccurred())
	return runtime.RawExtension{Raw: rawJSON}
}
