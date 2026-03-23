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
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// fixtureRGD describes an expected RGD fixture and what to validate.
type fixtureRGD struct {
	// Name is the RGD name.
	Name string
	// ExpectedInstanceGVR is the GVR of the generated CRD for instances.
	ExpectedInstanceGVR schema.GroupVersionResource
	// InstanceName is the name of the fixture instance to validate.
	InstanceName string
	// InstanceNamespace is the namespace of the fixture instance.
	InstanceNamespace string
	// ExpectedChildResources are the child resources that should exist.
	ExpectedChildResources []expectedChild
}

type expectedChild struct {
	GVR       schema.GroupVersionResource
	Name      string
	Namespace string
}

const legacyConditionResourceGraphAccepted = "ResourceGraphAccepted"

var (
	gvrNamespaces = schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "namespaces",
	}
	gvrAppsDeployments = schema.GroupVersionResource{
		Group: "apps", Version: "v1", Resource: "deployments",
	}
	gvrCoreServices = schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "services",
	}
	gvrCoreConfigMaps = schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "configmaps",
	}
	gvrCRDs = schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
	}

	kroGVR = func(resource string) schema.GroupVersionResource {
		return schema.GroupVersionResource{
			Group: "kro.run", Version: "v1alpha1", Resource: resource,
		}
	}
)

var fixtures = []fixtureRGD{
	{
		Name:                "upgrade-simple-deployment",
		ExpectedInstanceGVR: kroGVR("upgradesimpleapps"),
		InstanceName:        "test-simple",
		InstanceNamespace:   "upgrade-test",
		ExpectedChildResources: []expectedChild{
			{GVR: gvrAppsDeployments, Name: "test-simple-deployment", Namespace: "upgrade-test"},
			{GVR: gvrCoreServices, Name: "test-simple-service", Namespace: "upgrade-test"},
		},
	},
	{
		Name:                "upgrade-conditional",
		ExpectedInstanceGVR: kroGVR("upgradeconditionalapps"),
		InstanceName:        "test-conditional-off",
		InstanceNamespace:   "upgrade-test",
		ExpectedChildResources: []expectedChild{
			{GVR: gvrCoreConfigMaps, Name: "test-conditional-off-configmap", Namespace: "upgrade-test"},
			// monitoringConfig NOT expected (enableMonitoring=false)
		},
	},
	{
		Name:                "upgrade-conditional",
		ExpectedInstanceGVR: kroGVR("upgradeconditionalapps"),
		InstanceName:        "test-conditional-on",
		InstanceNamespace:   "upgrade-test",
		ExpectedChildResources: []expectedChild{
			{GVR: gvrCoreConfigMaps, Name: "test-conditional-on-configmap", Namespace: "upgrade-test"},
			{GVR: gvrCoreConfigMaps, Name: "test-conditional-on-monitoring", Namespace: "upgrade-test"},
		},
	},
	{
		Name:                "upgrade-multi-dependency",
		ExpectedInstanceGVR: kroGVR("upgrademultideps"),
		InstanceName:        "test-multidep",
		InstanceNamespace:   "upgrade-test",
		ExpectedChildResources: []expectedChild{
			{GVR: gvrCoreConfigMaps, Name: "test-multidep-config", Namespace: "upgrade-test"},
			{GVR: gvrAppsDeployments, Name: "test-multidep-deployment", Namespace: "upgrade-test"},
			// service name derived from ${deployment.metadata.name}
			{GVR: gvrCoreServices, Name: "test-multidep-deployment", Namespace: "upgrade-test"},
		},
	},
	{
		Name:                "upgrade-external-ref",
		ExpectedInstanceGVR: kroGVR("upgradeextrefs"),
		InstanceName:        "test-extref",
		InstanceNamespace:   "upgrade-test",
		ExpectedChildResources: []expectedChild{
			{GVR: gvrCoreConfigMaps, Name: "test-extref-configmap", Namespace: "upgrade-test"},
		},
	},
	{
		Name:                "upgrade-validation-markers",
		ExpectedInstanceGVR: kroGVR("upgradevalidateds"),
		InstanceName:        "test-validated",
		InstanceNamespace:   "upgrade-test",
		ExpectedChildResources: []expectedChild{
			{GVR: gvrCoreConfigMaps, Name: "test-validated-configmap", Namespace: "upgrade-test"},
		},
	},
	{
		Name:                "upgrade-readywhen-nil",
		ExpectedInstanceGVR: kroGVR("upgradereadynils"),
		InstanceName:        "test-readynil",
		InstanceNamespace:   "upgrade-test",
		ExpectedChildResources: []expectedChild{
			{GVR: gvrCoreConfigMaps, Name: "test-readynil-configmap", Namespace: "upgrade-test"},
		},
	},
	{
		Name:                "upgrade-readywhen-empty",
		ExpectedInstanceGVR: kroGVR("upgradereadyempties"),
		InstanceName:        "test-readyempty",
		InstanceNamespace:   "upgrade-test",
		ExpectedChildResources: []expectedChild{
			{GVR: gvrCoreConfigMaps, Name: "test-readyempty-configmap", Namespace: "upgrade-test"},
		},
	},
	{
		Name:                "upgrade-includewhen-nil",
		ExpectedInstanceGVR: kroGVR("upgradeincludenils"),
		InstanceName:        "test-includenil",
		InstanceNamespace:   "upgrade-test",
		ExpectedChildResources: []expectedChild{
			{GVR: gvrCoreConfigMaps, Name: "test-includenil-configmap", Namespace: "upgrade-test"},
		},
	},
	{
		Name:                "upgrade-includewhen-empty",
		ExpectedInstanceGVR: kroGVR("upgradeincludeempties"),
		InstanceName:        "test-includeempty",
		InstanceNamespace:   "upgrade-test",
		ExpectedChildResources: []expectedChild{
			{GVR: gvrCoreConfigMaps, Name: "test-includeempty-configmap", Namespace: "upgrade-test"},
		},
	},
	{
		Name:                "upgrade-mixed-nil-empty",
		ExpectedInstanceGVR: kroGVR("upgrademixednilempties"),
		InstanceName:        "test-mixed",
		InstanceNamespace:   "upgrade-test",
		ExpectedChildResources: []expectedChild{
			{GVR: gvrCoreConfigMaps, Name: "test-mixed-full", Namespace: "upgrade-test"},
			{GVR: gvrCoreConfigMaps, Name: "test-mixed-minimal", Namespace: "upgrade-test"},
		},
	},
	{
		Name:                "upgrade-group-default",
		ExpectedInstanceGVR: kroGVR("upgradegroupdefaults"),
		InstanceName:        "test-groupdefault",
		InstanceNamespace:   "upgrade-test",
		ExpectedChildResources: []expectedChild{
			{GVR: gvrCoreConfigMaps, Name: "test-groupdefault-configmap", Namespace: "upgrade-test"},
		},
	},
	{
		Name:                "upgrade-group-explicit",
		ExpectedInstanceGVR: kroGVR("upgradegroupexplicits"),
		InstanceName:        "test-groupexplicit",
		InstanceNamespace:   "upgrade-test",
		ExpectedChildResources: []expectedChild{
			{GVR: gvrCoreConfigMaps, Name: "test-groupexplicit-configmap", Namespace: "upgrade-test"},
		},
	},
	{
		Name:                "upgrade-metadata-nil",
		ExpectedInstanceGVR: kroGVR("upgrademetanils"),
		InstanceName:        "test-metanil",
		InstanceNamespace:   "upgrade-test",
		ExpectedChildResources: []expectedChild{
			{GVR: gvrCoreConfigMaps, Name: "test-metanil-configmap", Namespace: "upgrade-test"},
		},
	},
	{
		Name:                "upgrade-metadata-empty",
		ExpectedInstanceGVR: kroGVR("upgrademetaempties"),
		InstanceName:        "test-metaempty",
		InstanceNamespace:   "upgrade-test",
		ExpectedChildResources: []expectedChild{
			{GVR: gvrCoreConfigMaps, Name: "test-metaempty-configmap", Namespace: "upgrade-test"},
		},
	},
	{
		Name:                "upgrade-foreach-nil",
		ExpectedInstanceGVR: kroGVR("upgradeforeachnils"),
		InstanceName:        "test-foreachnil",
		InstanceNamespace:   "upgrade-test",
		ExpectedChildResources: []expectedChild{
			{GVR: gvrCoreConfigMaps, Name: "test-foreachnil-configmap", Namespace: "upgrade-test"},
		},
	},
	{
		Name:                "upgrade-foreach-empty",
		ExpectedInstanceGVR: kroGVR("upgradeforeachempties"),
		InstanceName:        "test-foreachempty",
		InstanceNamespace:   "upgrade-test",
		ExpectedChildResources: []expectedChild{
			{GVR: gvrCoreConfigMaps, Name: "test-foreachempty-configmap", Namespace: "upgrade-test"},
		},
	},
	{
		Name:                "upgrade-resources-reverse-order",
		ExpectedInstanceGVR: kroGVR("upgradereverseorders"),
		InstanceName:        "test-reverseorder",
		InstanceNamespace:   "upgrade-test",
		ExpectedChildResources: []expectedChild{
			{GVR: gvrCoreConfigMaps, Name: "test-reverseorder-aafirst", Namespace: "upgrade-test"},
			{GVR: gvrCoreConfigMaps, Name: "test-reverseorder-zzsecond", Namespace: "upgrade-test"},
		},
	},
	{
		Name:                "upgrade-deletion-target",
		ExpectedInstanceGVR: kroGVR("upgradedeletiontargets"),
		InstanceName:        "test-deletion-target",
		InstanceNamespace:   "upgrade-test",
		ExpectedChildResources: []expectedChild{
			{GVR: gvrCoreConfigMaps, Name: "test-deletion-target-configmap", Namespace: "upgrade-test"},
		},
	},
}

var _ = ginkgo.Describe("Shared Validation", ginkgo.Ordered, func() {
	ginkgo.It("should have all RGDs in Active state with conditions True", func() {
		gomega.Eventually(func(g gomega.Gomega) {
			rgdList := &krov1alpha1.ResourceGraphDefinitionList{}
			g.Expect(k8sClient.List(ctx, rgdList)).To(gomega.Succeed())
			g.Expect(rgdList.Items).NotTo(gomega.BeEmpty(), "Should have at least one RGD")

			for _, rgd := range rgdList.Items {
				g.Expect(rgd.Status.State).To(
					gomega.Equal(krov1alpha1.ResourceGraphDefinitionStateActive),
					"RGD %s should be Active, got %s", rgd.Name, rgd.Status.State,
				)

				// Check all non-legacy conditions are True. Skip ResourceGraphAccepted
				// which is a stale v0.8.x condition not updated by the new controller.
				for _, cond := range rgd.Status.Conditions {
					if string(cond.Type) == legacyConditionResourceGraphAccepted {
						continue
					}
					g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionTrue),
						"RGD %s condition %s should be True, got %s",
						rgd.Name, cond.Type, cond.Status)
				}
			}
		}, 3*time.Minute, 3*time.Second).Should(gomega.Succeed())
	})

	for _, f := range fixtures {
		ginkgo.It(fmt.Sprintf("should have instance %s/%s in ACTIVE state", f.InstanceNamespace, f.InstanceName), func() {
			obj, err := dynamicClient.Resource(f.ExpectedInstanceGVR).
				Namespace(f.InstanceNamespace).
				Get(ctx, f.InstanceName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred(),
				"Instance %s/%s should exist", f.InstanceNamespace, f.InstanceName)

			state, found, err := unstructured.NestedString(obj.Object, "status", "state")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(found).To(gomega.BeTrue(), "Instance should have status.state")
			gomega.Expect(state).To(gomega.Equal("ACTIVE"),
				"Instance %s/%s should be ACTIVE, got %s", f.InstanceNamespace, f.InstanceName, state)
		})

		for _, child := range f.ExpectedChildResources {
			ginkgo.It(fmt.Sprintf("should have child resource %s/%s (%s)",
				child.Namespace, child.Name, child.GVR.Resource), func() {
				_, err := dynamicClient.Resource(child.GVR).
					Namespace(child.Namespace).
					Get(ctx, child.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred(),
					"Child resource %s/%s (%s) should exist", child.Namespace, child.Name, child.GVR.Resource)
			})
		}
	}
})
