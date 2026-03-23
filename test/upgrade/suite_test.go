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
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
)

var (
	k8sClient     client.Client
	clientset     *kubernetes.Clientset
	dynamicClient dynamic.Interface
	ctx           context.Context
	scheme        = runtime.NewScheme()

	// Configuration from environment
	upgradeFromVersion = os.Getenv("KRO_UPGRADE_FROM_VERSION")
	upgradeMode        = os.Getenv("KRO_UPGRADE_MODE") // "pre-upgrade" or "post-upgrade"
	skipGRAssertions   = os.Getenv("KRO_UPGRADE_SKIP_GR_ASSERTIONS") == "true"
	artifactsDir       = os.Getenv("ARTIFACTS")

	// postUpgradeInitialGRCount captures GR count per RGD right after upgrade,
	// before any mutations. Set in BeforeSuite for post-upgrade mode.
	postUpgradeInitialGRCount map[string]int
)

func TestUpgrade(t *testing.T) {
	suiteName := fmt.Sprintf("Upgrade Test Suite [%s]", upgradeMode)
	if upgradeFromVersion != "" {
		suiteName = fmt.Sprintf("%s: %s -> current", suiteName, upgradeFromVersion)
	}
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, suiteName)
}

var _ = ginkgo.BeforeSuite(func() {
	if upgradeMode == "" {
		upgradeMode = "post-upgrade"
	}
	if artifactsDir == "" {
		artifactsDir = "_artifacts"
	}

	ginkgo.GinkgoLogr.Info("Starting upgrade test suite",
		"mode", upgradeMode,
		"upgradeFrom", upgradeFromVersion,
		"skipGRAssertions", skipGRAssertions,
	)

	gomega.Expect(clientgoscheme.AddToScheme(scheme)).To(gomega.Succeed())
	gomega.Expect(krov1alpha1.AddToScheme(scheme)).To(gomega.Succeed())

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	clientset, err = kubernetes.NewForConfig(cfg)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	dynamicClient, err = dynamic.NewForConfig(cfg)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	ctx = context.Background()

	// In pre-upgrade mode, apply fixtures and wait for them to be ready
	// before any test specs run.
	if isPreUpgrade() {
		setupFixtures()
	}

	// In post-upgrade mode, wait for controller to settle, then capture
	// initial GR counts before any mutation tests run.
	if isPostUpgrade() {
		waitForControllerReady()
		postUpgradeInitialGRCount = captureGRCounts()
	}
})

// waitForControllerReady waits for:
// 1. All kro CRDs to be Established
// 2. All RGDs to be Active with non-legacy conditions True
// 3. GraphRevision objects to exist for all RGDs
func waitForControllerReady() {
	ginkgo.GinkgoLogr.Info("Waiting for CRDs to be Established")
	gomega.Eventually(func(g gomega.Gomega) {
		for _, crdName := range []string{
			"resourcegraphdefinitions.kro.run",
			"graphrevisions.internal.kro.run",
		} {
			crd, err := dynamicClient.Resource(gvrCRDs).Get(ctx, crdName, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred(), "CRD %s should exist", crdName)

			conditions, found, _ := unstructured.NestedSlice(crd.Object, "status", "conditions")
			g.Expect(found).To(gomega.BeTrue(), "CRD %s should have conditions", crdName)

			established := false
			for _, c := range conditions {
				cond, ok := c.(map[string]interface{})
				if !ok {
					continue
				}
				if cond["type"] == "Established" && cond["status"] == "True" {
					established = true
					break
				}
			}
			g.Expect(established).To(gomega.BeTrue(), "CRD %s should be Established", crdName)
		}
	}, 2*time.Minute, 3*time.Second).Should(gomega.Succeed())

	ginkgo.GinkgoLogr.Info("Waiting for all RGDs to be Active with conditions True")
	gomega.Eventually(func(g gomega.Gomega) {
		rgdList := &krov1alpha1.ResourceGraphDefinitionList{}
		g.Expect(k8sClient.List(ctx, rgdList)).To(gomega.Succeed())
		g.Expect(rgdList.Items).NotTo(gomega.BeEmpty())

		for _, rgd := range rgdList.Items {
			g.Expect(rgd.Status.State).To(
				gomega.Equal(krov1alpha1.ResourceGraphDefinitionStateActive),
				"RGD %s not Active yet", rgd.Name)

			for _, cond := range rgd.Status.Conditions {
				if string(cond.Type) == legacyConditionResourceGraphAccepted {
					continue
				}
				g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionTrue),
					"RGD %s condition %s not True yet", rgd.Name, cond.Type)
			}
		}
	}, 3*time.Minute, 3*time.Second).Should(gomega.Succeed())

	ginkgo.GinkgoLogr.Info("Controller ready, all RGDs Active")
}

func isPreUpgrade() bool {
	return upgradeMode == "pre-upgrade"
}

func isPostUpgrade() bool {
	return upgradeMode == "post-upgrade"
}
