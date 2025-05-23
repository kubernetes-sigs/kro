// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package framework

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
)

const (
	// TestDataDir is the base directory for test data
	TestDataDir = "testdata"
	// InputsDir is the directory containing step inputs
	InputsDir = "inputs"
	// OutputsDir is the directory containing step outputs
	OutputsDir = "outputs"
	// ScenarioFile is the name of the scenario description file
	ScenarioFile = "scenario.txt"
)

// TestCase represents a single e2e test case
type TestCase struct {
	// Name is the name of the test case
	Name string
	// Framework is the test framework
	Framework *Framework
	// Namespace is the test namespace
	Namespace string
	// TestDataPath is the path to the test data directory
	TestDataPath string
	// t is the testing.T instance
	t *testing.T
	// timeout is the timeout for test operations
	timeout time.Duration
	// clusterScopedObjects tracks cluster-scoped objects created during the test
	clusterScopedObjects []objectRef
}

// objectRef holds a reference to a Kubernetes object
type objectRef struct {
	GVK  schema.GroupVersionKind
	Name string
}

// trackClusterScopedObject adds a cluster-scoped object to the tracking list
func (tc *TestCase) trackClusterScopedObject(obj *unstructured.Unstructured) {
	tc.clusterScopedObjects = append(tc.clusterScopedObjects, objectRef{
		GVK:  obj.GroupVersionKind(),
		Name: obj.GetName(),
	})
}

// NewTestCase creates a new test case
func NewTestCase(t *testing.T, f *Framework, name string) (*TestCase, error) {
	t.Helper()

	// Enable parallel test execution
	//t.Parallel()

	// Create test case
	tc := &TestCase{
		Name:         name,
		TestDataPath: filepath.Join("../../../..", "test", "e2e", "testdata", name),
		t:            t,
		timeout:      DefaultWaitTimeout, // default timeout
		Framework:    f,
	}

	// Create test namespace
	if err := tc.createNamespace(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to create namespace: %w", err)
	}

	tc.t.Logf("Using namespace: %s, from: testdata/%s", tc.Namespace, tc.TestDataPath)
	// Register cleanup
	t.Cleanup(func() {
		ctx := context.Background()
		tc.Cleanup(ctx)
	})

	return tc, nil
}

// RunTest runs the test case with the given test function
func (tc *TestCase) RunTest(t *testing.T, fn func(ctx context.Context) error) {
	t.Helper()
	ctx := context.Background()

	// Run test
	err := fn(ctx)
	require.NoError(t, err)
}

// Cleanup deletes the test case namespace and cluster-scoped objects
func (tc *TestCase) Cleanup(ctx context.Context) {
	// Delete namespace first (this will delete all namespace-scoped objects)
	if tc.Namespace != "" {
		ns := &unstructured.Unstructured{}
		ns.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "",
			Version: "v1",
			Kind:    "Namespace",
		})
		ns.SetName(tc.Namespace)
		tc.t.Logf("Deleting namespace: %s", tc.Namespace)
		if err := tc.Framework.Client.Delete(ctx, ns); err != nil && !errors.IsNotFound(err) {
			tc.t.Logf("Failed Deleting namespace: %s", tc.Namespace)
		}
	}

	// Delete cluster-scoped objects in reverse order
	// This ensures dependencies are respected (e.g., CRDs are deleted after their instances)
	for i := len(tc.clusterScopedObjects) - 1; i >= 0; i-- {
		ref := tc.clusterScopedObjects[i]
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(ref.GVK)
		obj.SetName(ref.Name)

		tc.t.Logf("Deleting cluster-scoped object: %s/%s", ref.GVK.Kind, ref.Name)
		if err := tc.Framework.Client.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
			tc.t.Logf("Failed to delete cluster-scoped object %s/%s: %v", ref.GVK.Kind, ref.Name, err)
		}
	}
}

// getSteps returns all step directories sorted by step number
func (tc *TestCase) getSteps() ([]string, error) {
	entries, err := os.ReadDir(tc.TestDataPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read test data directory: %w", err)
	}

	var steps []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if strings.HasPrefix(entry.Name(), "step") {
			steps = append(steps, entry.Name())
		}
	}

	// Sort steps by number
	sort.Slice(steps, func(i, j int) bool {
		numI, _ := strconv.Atoi(strings.TrimPrefix(steps[i], "step"))
		numJ, _ := strconv.Atoi(strings.TrimPrefix(steps[j], "step"))
		return numI < numJ
	})

	return steps, nil
}

// printScenario prints the scenario description if it exists
func (tc *TestCase) printScenario(stepDir string) error {
	scenarioPath := filepath.Join(tc.TestDataPath, stepDir, ScenarioFile)
	content, err := os.ReadFile(scenarioPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read scenario file: %w", err)
	}

	tc.t.Logf("Running %s: %s", stepDir, string(content))
	return nil
}

// RunStep runs a single test step
func (tc *TestCase) RunStep(ctx context.Context, step string) error {
	// Print scenario if exists
	if err := tc.printScenario(step); err != nil {
		return err
	}

	// Apply inputs
	inputsDir := filepath.Join(tc.TestDataPath, step, InputsDir)
	if err := tc.applyDirectoryWithOrder(ctx, inputsDir); err != nil {
		return fmt.Errorf("failed to apply inputs for step %s: %w", step, err)
	}

	// Verify outputs
	outputsDir := filepath.Join(tc.TestDataPath, step, OutputsDir)
	if err := tc.VerifyOutputsWithTimeout(ctx, tc.timeout, outputsDir); err != nil {
		return fmt.Errorf("failed to verify outputs for step %s: %w", step, err)
	}

	return nil
}

// RunAllSteps runs all steps in order
func (tc *TestCase) RunAllSteps(ctx context.Context) error {
	steps, err := tc.getSteps()
	if err != nil {
		return err
	}

	if len(steps) == 0 {
		return fmt.Errorf("no steps found in test case %s", tc.Name)
	}

	// Verify step0 exists
	if steps[0] != "step0" {
		return fmt.Errorf("step0 not found in test case %s", tc.Name)
	}

	// Run all steps in order
	for _, step := range steps {
		if err := tc.RunStep(ctx, step); err != nil {
			return fmt.Errorf("failed to run step %s: %w", step, err)
		}
	}

	return nil
}

// createNamespace creates a unique namespace for the test case
func (tc *TestCase) createNamespace(ctx context.Context) error {
	ns := &unstructured.Unstructured{}
	ns.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Namespace",
	})
	ns.SetName(fmt.Sprintf("e2e-%s-%d", tc.Name, time.Now().Unix()))

	if err := tc.Framework.Client.Create(ctx, ns); err != nil {
		return fmt.Errorf("failed to create namespace: %w", err)
	}

	tc.Namespace = ns.GetName()
	return nil
}

// SortedLoadObjectsFromDirectory loads objects from a directory and sorts them by namespace and filename
// Uses discoverClient to identify cluster-scoped resources and namespace scoped resources.
// For namespace scoped resources, it sets the namespace to the test case namespace.
func (tc *TestCase) SortedLoadObjectsFromDirectory(ctx context.Context, dirPath string) ([]resourceInfo, error) {
	// Create discovery client to check if resources are cluster-scoped
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(tc.Framework.RESTConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create discovery client: %w", err)
	}

	// Get all resources from the API server
	apiResources, err := discoveryClient.ServerPreferredResources()
	if err != nil {
		return nil, fmt.Errorf("failed to get server resources: %w", err)
	}

	// Create a map of GVK to scope (cluster or namespaced)
	scopeMap := make(map[schema.GroupVersionKind]bool)
	for _, list := range apiResources {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range list.APIResources {
			gvk := schema.GroupVersionKind{
				Group:   gv.Group,
				Version: gv.Version,
				Kind:    r.Kind,
			}
			scopeMap[gvk] = !r.Namespaced
		}
	}

	// Load resources from directory
	resources, err := LoadObjectsFromDirectory(dirPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load objects: %w", err)
	}

	// Set cluster-scoped flag
	for i := range resources {
		// Determine if resource is cluster-scoped
		gvk := resources[i].obj.GroupVersionKind()
		isClusterScoped := wellKnownClusterScopedKinds.Has(gvk.Kind)
		if !isClusterScoped {
			if scope, ok := scopeMap[gvk]; ok {
				isClusterScoped = scope
			}
		}
		resources[i].isClusterScoped = isClusterScoped

		// Set namespace for namespaced resources
		if !isClusterScoped {
			resources[i].obj.SetNamespace(tc.Namespace)
		}
	}

	// Sort resources:
	// 1. Cluster-scoped resources first
	// 2. For same scope, sort by filename
	sort.Slice(resources, func(i, j int) bool {
		if resources[i].isClusterScoped != resources[j].isClusterScoped {
			return resources[i].isClusterScoped
		}
		return resources[i].filename < resources[j].filename
	})

	return resources, nil
}

// applyDirectoryWithOrder reads and applies Kubernetes manifests from a directory with proper ordering
func (tc *TestCase) applyDirectoryWithOrder(ctx context.Context, dirPath string) error {

	resources, err := tc.SortedLoadObjectsFromDirectory(ctx, dirPath)
	if err != nil {
		return fmt.Errorf("failed to load objects: %w", err)
	}

	// Create dynamic client
	dynamicClient, err := dynamic.NewForConfig(tc.Framework.RESTConfig)
	if err != nil {
		return fmt.Errorf("failed to create dynamic client: %w", err)
	}

	// Create discovery client to check if resources are cluster-scoped
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(tc.Framework.RESTConfig)
	if err != nil {
		return fmt.Errorf("failed to create discovery client: %w", err)
	}

	// Create REST mapper
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient))
	// Apply resources
	for _, res := range resources {
		obj := res.obj

		tc.t.Logf("   Applying %s", res.String())
		// Get GVR for the object
		gvk := obj.GroupVersionKind()
		mapping, err := restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return fmt.Errorf("failed to get REST mapping for %s: %w", gvk, err)
		}

		// Create or update the resource
		var dr dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			dr = dynamicClient.Resource(mapping.Resource).Namespace(obj.GetNamespace())
		} else {
			dr = dynamicClient.Resource(mapping.Resource)
		}

		_, err = dr.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{
			FieldManager: "kro-e2e-test",
			Force:        true,
		})
		if err != nil {
			return fmt.Errorf("failed to apply resource %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
		if res.isClusterScoped {
			tc.trackClusterScopedObject(obj)
		}
	}

	return nil
}

// WithTimeout sets the timeout for test operations
func (tc *TestCase) WithTimeout(timeout time.Duration) *TestCase {
	tc.timeout = timeout
	return tc
}
