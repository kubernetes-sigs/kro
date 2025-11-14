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

package deployment_test

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

var (
	similarObjectAssertions = map[string]func(
		t *testing.T,
		ref ResourceRef,
		helmObject, kustomizeObject *unstructured.Unstructured,
	){
		// kube-builder does not respect a strict order and hence we have no way
		// to ensure the order of the rules are the same between helm and kustomize.
		"ClusterRole":              assertClusterRoleObjectsAreSimilar,
		"CustomResourceDefinition": assertCustomResourceDefinitionObjectsAreSimilar,
	}
)

func renderHelmDeployment(t *testing.T, args ...string) map[ResourceRef]*unstructured.Unstructured {
	t.Helper()
	out := bytes.Buffer{}
	cmd := exec.Command("helm", append([]string{
		"template", "kro",
		"--create-namespace", "--include-crds",
		"--namespace", "kro-system",
		"../../helm",
		"--version", "v0.0.0-test-integration",
		"--set", "image.tag=v0.0.0-test-integration",
	}, args...)...)
	cmd.Stderr = os.Stderr
	cmd.Stdout = &out
	require.NoError(t, cmd.Run())

	objects, err := ParseUnstructured(bufio.NewReader(&out))
	require.NoError(t, err)
	return objects
}

func renderKustomizeDeployment(t *testing.T, path string) map[ResourceRef]*unstructured.Unstructured {
	t.Helper()
	out := bytes.Buffer{}
	cmd := exec.Command("kustomize", "build", path)
	cmd.Stderr = os.Stderr
	cmd.Stdout = &out
	require.NoError(t, cmd.Run())

	objects, err := ParseUnstructured(bufio.NewReader(&out))
	require.NoError(t, err)
	return objects
}

func removeDivergingFields(object *unstructured.Unstructured) {
	for _, divergingLabel := range []string{
		"helm.sh/chart",
		"app.kubernetes.io/managed-by",
		"app.kubernetes.io/version",
	} {
		unstructured.RemoveNestedField(object.Object,
			"metadata", "labels", divergingLabel)
		unstructured.RemoveNestedField(object.Object,
			"spec", "template", "metadata", "labels", divergingLabel)
	}
}

func assertObjectsAreSimilar(t *testing.T, ref ResourceRef, helmObject, kustomizeObject *unstructured.Unstructured) {
	t.Helper()
	// Those fields are specific to the deployment methods and already validated.
	// ignore them

	removeDivergingFields(helmObject)
	removeDivergingFields(kustomizeObject)

	assert.Equal(t, helmObject.Object, kustomizeObject.Object, "objects %s are not similar", ref.String())
}

func assertClusterRoleObjectsAreSimilar(
	t *testing.T,
	ref ResourceRef,
	helmObject, kustomizeObject *unstructured.Unstructured,
) {
	t.Helper()
	removeDivergingFields(helmObject)
	removeDivergingFields(kustomizeObject)

	helmClusterRole := rbacv1.ClusterRole{}
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(helmObject.Object, &helmClusterRole)
	require.NoError(t, err)

	kustomizeClusterRole := rbacv1.ClusterRole{}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(kustomizeObject.Object, &kustomizeClusterRole)
	require.NoError(t, err)

	// We should be insensitive to the order of the rules and the different fields
	for _, rule := range helmClusterRole.Rules {
		slices.Sort(rule.Verbs)
		slices.Sort(rule.APIGroups)
		slices.Sort(rule.Resources)
		slices.Sort(rule.NonResourceURLs)
	}
	for _, rule := range kustomizeClusterRole.Rules {
		slices.Sort(rule.Verbs)
		slices.Sort(rule.APIGroups)
		slices.Sort(rule.Resources)
		slices.Sort(rule.NonResourceURLs)
	}

	slices.SortFunc(helmClusterRole.Rules, func(a, b rbacv1.PolicyRule) int {
		return strings.Compare(strings.Join(a.APIGroups, ","), strings.Join(b.APIGroups, ","))
	})

	slices.SortFunc(kustomizeClusterRole.Rules, func(a, b rbacv1.PolicyRule) int {
		return strings.Compare(strings.Join(a.APIGroups, ","), strings.Join(b.APIGroups, ","))
	})

	assert.Equal(t, helmClusterRole, kustomizeClusterRole, "ClusterRole %s are not similar", ref.String())
}

func assertCustomResourceDefinitionObjectsAreSimilar(
	t *testing.T,
	ref ResourceRef,
	helmObject, kustomizeObject *unstructured.Unstructured,
) {
	t.Helper()
	unstructured.RemoveNestedField(kustomizeObject.Object,
		"metadata", "labels")
	unstructured.RemoveNestedField(helmObject.Object,
		"metadata", "labels")

	assert.Equal(t, helmObject.Object, kustomizeObject.Object, "CustomResourceDefinition %s are not similar", ref.String())
}

func assertSimilarHelmAndKustomizeObjects(
	t *testing.T,
	helmObjects,
	kustomizeObjects map[ResourceRef]*unstructured.Unstructured,
) {
	t.Helper()
	for ref := range kustomizeObjects {
		if ref.Kind == "Namespace" {
			// TODO: manage to include namespaces in the helm template
			// currently, they are not included despite --create-namespace
			continue
		}
		assert.Contains(t, helmObjects, ref, "object %s is missing from helm", ref.String())
	}
	for ref, helmObject := range helmObjects {
		assert.Contains(t, kustomizeObjects, ref, "object %s is missing from kustomize", ref.String())
		kustomizeObject, ok := kustomizeObjects[ref]
		if !ok {
			// We have already reported the problem above
			continue
		}
		t.Run(ref.String(), func(t *testing.T) {
			assertFunc, ok := similarObjectAssertions[ref.Kind]
			if ok {
				assertFunc(t, ref, helmObject.DeepCopy(), kustomizeObject.DeepCopy())
			} else {
				assertObjectsAreSimilar(t, ref, helmObject.DeepCopy(), kustomizeObject.DeepCopy())
			}
		})
	}
}

func assertAllObjectsAreManagedBy(
	t *testing.T,
	objects map[ResourceRef]*unstructured.Unstructured,
	expectedManagedBy string,
) {
	t.Helper()
	for ref, obj := range objects {
		t.Run(fmt.Sprintf("%s must be managed by %s", ref.String(), expectedManagedBy), func(t *testing.T) {
			if ref.Kind == "CustomResourceDefinition" && expectedManagedBy == "Helm" {
				t.Skip("CRDs are not templates in Helm and can't have the version label")
			}
			managedBy, found, err := unstructured.NestedString(obj.Object,
				"metadata", "labels", "app.kubernetes.io/managed-by",
			)
			require.NoError(t, err)
			require.True(t, found, "could not find the managed-by label")
			assert.Equal(t, expectedManagedBy, managedBy, "object %s is not managed by %s", ref.String(), expectedManagedBy)
		})

		template, found, err := unstructured.NestedMap(obj.Object, "spec", "template")
		assert.NoError(t, err)
		if !found || err != nil {
			continue
		}

		t.Run(fmt.Sprintf("%s template must be managed by %s", ref.String(), expectedManagedBy), func(t *testing.T) {

			templateManagedBy, found, err := unstructured.NestedString(template,
				"metadata", "labels", "app.kubernetes.io/managed-by",
			)
			require.NoError(t, err)
			require.True(t, found, "could not find the managed-by label in the template")
			assert.Equal(t, expectedManagedBy, templateManagedBy,
				"template %s is not managed by %s", ref.String(), expectedManagedBy)
		})
	}
}

func assertAllObjectsAreTaggedWithVersion(
	t *testing.T,
	objects map[ResourceRef]*unstructured.Unstructured,
	expectedVersion string,
) {
	t.Helper()
	for ref, obj := range objects {
		t.Run(fmt.Sprintf("%s must be tagged with version %s", ref.String(), expectedVersion), func(t *testing.T) {
			if ref.Kind == "CustomResourceDefinition" {
				t.Skip("CRDs are not templates in Helm and can't have the version label")
			}

			version, found, err := unstructured.NestedString(obj.Object,
				"metadata", "labels", "app.kubernetes.io/version",
			)
			require.NoError(t, err)
			require.True(t, found, "could not find the version label")
			assert.Equal(t, expectedVersion, version, "object %s is not tagged with version %s", ref.String(), expectedVersion)

			_, found, err = unstructured.NestedString(obj.Object,
				"spec", "selector", "app.kubernetes.io/version",
			)
			require.NoError(t, err)
			assert.False(t, found,
				"object %s selector should not be tagged with version. selectors are immutable",
				ref.String(),
			)

			template, found, err := unstructured.NestedMap(obj.Object, "spec", "template")
			require.NoError(t, err)
			if !found {
				return
			}

			templateVersion, found, err := unstructured.NestedString(template, "metadata", "labels", "app.kubernetes.io/version")
			require.NoError(t, err)
			require.True(t, found, "could not find the version label in the template")
			assert.Equal(t, expectedVersion, templateVersion,
				"template %s is not tagged with version %s", ref.String(), expectedVersion)

		})
	}
}

func TestHelmAndKustomizeDeploymentsMustBeSimilar(t *testing.T) {
	t.Run("kustomize kro core install should be similar to helm one", func(t *testing.T) {
		helmObjects := renderHelmDeployment(t, "--set", "rbac.mode=aggregation")
		kustomizeObjects := renderKustomizeDeployment(t, "./manifests/core-install")

		assertAllObjectsAreManagedBy(t, helmObjects, "Helm")
		assertAllObjectsAreManagedBy(t, kustomizeObjects, "kustomize")

		// version is injected by Chart.yaml.
		// We won't update it for tests
		assertAllObjectsAreTaggedWithVersion(t, helmObjects, "0.0.0-dev")
		assertAllObjectsAreTaggedWithVersion(t, kustomizeObjects, "0.0.0-test-integration")

		assertSimilarHelmAndKustomizeObjects(t, helmObjects, kustomizeObjects)
	})

	t.Run("kustomize kro core install with prometheus should be similar to helm one", func(t *testing.T) {
		helmObjects := renderHelmDeployment(t,
			"--set", "rbac.mode=aggregation",
			"--set", "metrics.service.create=true",
			"--set", "metrics.serviceMonitor.enabled=true",
		)
		kustomizeObjects := renderKustomizeDeployment(t, "./manifests/core-install-with-prometheus")

		assertAllObjectsAreManagedBy(t, helmObjects, "Helm")
		assertAllObjectsAreManagedBy(t, kustomizeObjects, "kustomize")

		// version is injected by Chart.yaml.
		// We won't update it for tests
		assertAllObjectsAreTaggedWithVersion(t, helmObjects, "0.0.0-dev")
		assertAllObjectsAreTaggedWithVersion(t, kustomizeObjects, "0.0.0-test-integration")

		assertSimilarHelmAndKustomizeObjects(t, helmObjects, kustomizeObjects)
	})

	t.Run("kustomize kro unrestricted install should be similar to helm one", func(t *testing.T) {
		helmObjects := renderHelmDeployment(t,
			"--set", "rbac.mode=unrestricted",
			"--set", "rbac.legacy=true",
		)
		kustomizeObjects := renderKustomizeDeployment(t, "./manifests/unrestricted")

		assertAllObjectsAreManagedBy(t, helmObjects, "Helm")
		assertAllObjectsAreManagedBy(t, kustomizeObjects, "kustomize")

		// version is injected by Chart.yaml.
		// We won't update it for tests
		assertAllObjectsAreTaggedWithVersion(t, helmObjects, "0.0.0-dev")
		assertAllObjectsAreTaggedWithVersion(t, kustomizeObjects, "0.0.0-test-integration")

		assertSimilarHelmAndKustomizeObjects(t, helmObjects, kustomizeObjects)
	})

	t.Run("kustomize kro unrestricted install with prometheus should be similar to helm one", func(t *testing.T) {
		helmObjects := renderHelmDeployment(t,
			"--set", "rbac.mode=unrestricted",
			"--set", "rbac.legacy=true",
			"--set", "metrics.service.create=true",
			"--set", "metrics.serviceMonitor.enabled=true",
		)
		kustomizeObjects := renderKustomizeDeployment(t, "./manifests/unrestricted-with-prometheus")

		assertAllObjectsAreManagedBy(t, helmObjects, "Helm")
		assertAllObjectsAreManagedBy(t, kustomizeObjects, "kustomize")

		// version is injected by Chart.yaml.
		// We won't update it for tests
		assertAllObjectsAreTaggedWithVersion(t, helmObjects, "0.0.0-dev")
		assertAllObjectsAreTaggedWithVersion(t, kustomizeObjects, "0.0.0-test-integration")

		assertSimilarHelmAndKustomizeObjects(t, helmObjects, kustomizeObjects)
	})
}
