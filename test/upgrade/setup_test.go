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
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gobuffalo/flect"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
)

const (
	fixturesDir  = "fixtures"
	setupTimeout = 5 * time.Minute
	setupPoll    = 3 * time.Second
)

// setupFixtures applies all fixture RGDs and instances, waiting for each
// stage to be ready. Called from BeforeSuite in pre-upgrade mode.
func setupFixtures() {
	ginkgo.By("Applying prerequisite resources (namespace, external configmap)")
	applyYAMLFile(filepath.Join(fixturesDir, "00-namespace.yaml"))
	applyYAMLFile(filepath.Join(fixturesDir, "01-external-configmap.yaml"))

	ginkgo.By("Applying all RGDs")
	dirs, err := os.ReadDir(fixturesDir)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		rgdPath := filepath.Join(fixturesDir, dir.Name(), "rgd.yaml")
		if _, err := os.Stat(rgdPath); os.IsNotExist(err) {
			continue
		}
		ginkgo.GinkgoLogr.Info("Applying RGD", "fixture", dir.Name())
		applyYAMLFile(rgdPath)
	}

	ginkgo.By("Waiting for all RGDs to become Active")
	gomega.Eventually(func(g gomega.Gomega) {
		rgdList := &krov1alpha1.ResourceGraphDefinitionList{}
		g.Expect(k8sClient.List(ctx, rgdList)).To(gomega.Succeed())
		g.Expect(rgdList.Items).NotTo(gomega.BeEmpty(), "Should have RGDs")

		for _, rgd := range rgdList.Items {
			g.Expect(rgd.Status.State).To(
				gomega.Equal(krov1alpha1.ResourceGraphDefinitionStateActive),
				"RGD %s should be Active, got %s", rgd.Name, rgd.Status.State)
		}
	}, setupTimeout, setupPoll).Should(gomega.Succeed())
	ginkgo.GinkgoLogr.Info("All RGDs are Active")

	ginkgo.By("Applying all instances")
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		instancePath := filepath.Join(fixturesDir, dir.Name(), "instance.yaml")
		if _, err := os.Stat(instancePath); os.IsNotExist(err) {
			continue
		}
		ginkgo.GinkgoLogr.Info("Applying instance", "fixture", dir.Name())
		applyYAMLFile(instancePath)
	}

	ginkgo.By("Waiting for all instances to become ACTIVE")
	for _, f := range fixtures {
		gomega.Eventually(func(g gomega.Gomega) {
			obj, err := dynamicClient.Resource(f.ExpectedInstanceGVR).
				Namespace(f.InstanceNamespace).
				Get(ctx, f.InstanceName, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred(),
				"Instance %s/%s should exist", f.InstanceNamespace, f.InstanceName)

			state, found, err := unstructured.NestedString(obj.Object, "status", "state")
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(found).To(gomega.BeTrue(), "Instance %s should have status.state", f.InstanceName)
			g.Expect(state).To(gomega.Equal("ACTIVE"),
				"Instance %s/%s should be ACTIVE, got %s", f.InstanceNamespace, f.InstanceName, state)
		}, setupTimeout, setupPoll).Should(gomega.Succeed(),
			"Timed out waiting for instance %s/%s to become ACTIVE", f.InstanceNamespace, f.InstanceName)
	}
	ginkgo.GinkgoLogr.Info("All instances are ACTIVE")
}

// applyYAMLFile reads a YAML file (possibly multi-doc) and applies each
// document to the cluster using the dynamic client.
func applyYAMLFile(path string) {
	data, err := os.ReadFile(path)
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), "Should read %s", path)

	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		obj := &unstructured.Unstructured{}
		err := decoder.Decode(obj)
		if err == io.EOF {
			break
		}
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "Should decode YAML from %s", path)

		if obj.Object == nil {
			continue
		}

		gvr := gvrFromUnstructured(obj)
		ns := obj.GetNamespace()

		if ns != "" {
			_, err = dynamicClient.Resource(gvr).Namespace(ns).
				Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{FieldManager: "upgrade-test"})
		} else {
			_, err = dynamicClient.Resource(gvr).
				Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{FieldManager: "upgrade-test"})
		}
		gomega.Expect(err).NotTo(gomega.HaveOccurred(),
			"Should apply %s %s/%s from %s", obj.GetKind(), ns, obj.GetName(), path)
	}
}

// gvrFromUnstructured derives the GVR for an unstructured object.
func gvrFromUnstructured(obj *unstructured.Unstructured) schema.GroupVersionResource {
	gvk := obj.GroupVersionKind()

	resourceMap := map[schema.GroupVersionKind]schema.GroupVersionResource{
		{Group: "", Version: "v1", Kind: "Namespace"}:      gvrNamespaces,
		{Group: "", Version: "v1", Kind: "ConfigMap"}:      gvrCoreConfigMaps,
		{Group: "", Version: "v1", Kind: "Service"}:        gvrCoreServices,
		{Group: "apps", Version: "v1", Kind: "Deployment"}: gvrAppsDeployments,
		{Group: "kro.run", Version: "v1alpha1", Kind: "ResourceGraphDefinition"}: {
			Group: "kro.run", Version: "v1alpha1", Resource: "resourcegraphdefinitions",
		},
	}

	if gvr, ok := resourceMap[gvk]; ok {
		return gvr
	}

	// For generated CRD instances, derive resource name using flect
	// (same pluralization kro uses internally).
	resource := flect.Pluralize(strings.ToLower(gvk.Kind))
	return schema.GroupVersionResource{
		Group:    gvk.Group,
		Version:  gvk.Version,
		Resource: resource,
	}
}
