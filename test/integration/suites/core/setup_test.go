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

package core_test

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	ctrlinstance "github.com/kubernetes-sigs/kro/pkg/controller/instance"
	"github.com/kubernetes-sigs/kro/test/integration/environment"
)

var env *environment.Environment

func TestCore(t *testing.T) {
	defaultMode := krov1alpha1.ResourceGraphDefinitionReconcileMode(os.Getenv("KRO_DEFAULT_RGD_RECONCILE_MODE"))
	RegisterFailHandler(Fail)
	BeforeSuite(func() {
		Expect(defaultMode).To(
			Or(
				BeEquivalentTo(krov1alpha1.ResourceGraphDefinitionReconcileModeClientSideDelta),
				BeEquivalentTo(krov1alpha1.ResourceGraphDefinitionReconcileModeApplySet),
			), "KRO_DEFAULT_RGD_RECONCILE_MODE must be a valid and recognized reconcile mode",
		)

		var err error
		env, err = environment.New(t.Context(),
			environment.ControllerConfig{
				AllowCRDDeletion: true,
				ReconcileConfig: ctrlinstance.ReconcileConfig{
					DefaultRequeueDuration: 5 * time.Second,
					Mode:                   defaultMode,
				},
				LogWriter: GinkgoWriter,
			},
		)
		Expect(err).NotTo(HaveOccurred())
	})
	AfterSuite(func() {
		err := (func() (err error) {
			// Need to sleep if the first stop fails due to a bug:
			// https://github.com/kubernetes-sigs/controller-runtime/issues/1571
			sleepTime := 1 * time.Millisecond
			for i := 0; i < 12; i++ { // Exponentially sleep up to ~4s
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

	RunSpecs(t, "Core Suite", Label(string(defaultMode)))
}

// Helper function to convert map to runtime.RawExtension
func toRawExtension(v interface{}) runtime.RawExtension {
	rawJSON, err := json.Marshal(v)
	Expect(err).NotTo(HaveOccurred())
	return runtime.RawExtension{Raw: rawJSON}
}
