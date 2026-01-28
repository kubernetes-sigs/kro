// Copyright 2026 The Kubernetes Authors.
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

package test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/kube-openapi/pkg/validation/spec"

	// Kro Imports
	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/ast"
	"github.com/kubernetes-sigs/kro/pkg/graph/parser"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
)

// Expectations defines the "expected" part of the test file
// UPDATED: Now supports the 'spec' nesting
type Expectations struct {
	Spec struct {
		Result  string `json:"result"`
		Message struct {
			Contains string `json:"contains"`
		} `json:"message"`
	} `json:"spec"`
}

var TestCmd = &cobra.Command{
	Use:   "test [test-directory]",
	Short: "Run local tests against a ResourceGraphDefinition",
	Args:  cobra.ExactArgs(1),
	RunE:  runTest,
}

func init() {
	TestCmd.Flags().StringP("filename", "f", "", "Path to the ResourceGraphDefinition file")
	_ = TestCmd.MarkFlagRequired("filename")
}

func runTest(cmd *cobra.Command, args []string) error {
	rgdPath, _ := cmd.Flags().GetString("filename")
	testDir := args[0]

	rgdData, err := os.ReadFile(rgdPath)
	if err != nil {
		return fmt.Errorf("failed to read RGD file: %w", err)
	}

	var rgd v1alpha1.ResourceGraphDefinition
	if err := yaml.Unmarshal(rgdData, &rgd); err != nil {
		return fmt.Errorf("failed to parse RGD: %w", err)
	}
	fmt.Printf("📦 Loaded RGD: %s\n", rgd.Name)

	return filepath.Walk(testDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && (strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")) {
			return runSingleTest(&rgd, path)
		}
		return nil
	})
}

func runSingleTest(rgd *v1alpha1.ResourceGraphDefinition, testPath string) error {
	fmt.Printf("  • Testing %s... ", filepath.Base(testPath))

	data, err := os.ReadFile(testPath)
	if err != nil {
		return err
	}

	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var expectations Expectations
	var instance unstructured.Unstructured
	var expectedResources []*unstructured.Unstructured

	if err := decoder.Decode(&expectations); err != nil {
		return fmt.Errorf("bad expectations: %v", err)
	}
	if err := decoder.Decode(&instance); err != nil {
		return fmt.Errorf("bad instance input: %v", err)
	}
	for {
		var res unstructured.Unstructured
		if err := decoder.Decode(&res); err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		if len(res.Object) > 0 {
			expectedResources = append(expectedResources, &res)
		}
	}

	mockInstance, mockResources, topoOrder, err := buildGraphOffline(rgd, &instance)
	if err != nil {
		return fmt.Errorf("failed to build offline graph: %w", err)
	}

	rt, err := runtime.NewResourceGraphDefinitionRuntime(
		mockInstance,
		mockResources,
		topoOrder,
	)
	if err != nil {
		return fmt.Errorf("failed to init runtime: %w", err)
	}

	rt.SetInstance(&instance)

	maxIterations := 5
	for i := 0; i < maxIterations; i++ {
		retry, err := rt.Synchronize()
		if err != nil {
			// UPDATED: Use expectations.Spec.Result
			if expectations.Spec.Result == "Failure" {
				if strings.Contains(err.Error(), expectations.Spec.Message.Contains) {
					fmt.Println("PASS (Expected Failure)")
					return nil
				}
				return fmt.Errorf("FAIL: got error %v, but expected error containing '%s'", err, expectations.Spec.Message.Contains)
			}
			return fmt.Errorf("runtime error: %w", err)
		}
		if !retry {
			break
		}
	}

	var generatedResources []*unstructured.Unstructured
	for _, id := range topoOrder {
		res, state := rt.GetResource(id)
		if state == runtime.ResourceStateResolved && res != nil {
			generatedResources = append(generatedResources, res)
		}
	}

	// UPDATED: Use expectations.Spec.Result
	if expectations.Spec.Result == "Success" {
		if len(generatedResources) != len(expectedResources) {
			fmt.Printf("FAIL\n    Expected %d resources, got %d\n", len(expectedResources), len(generatedResources))
			return nil
		}
		fmt.Println("PASS")
	} else {
		fmt.Printf("FAIL (Expected Failure but got Success)\n")
	}

	return nil
}

// --- Offline Graph Construction Logic ---

type MockResource struct {
	ID          string
	Obj         *unstructured.Unstructured
	Vars        []*variable.ResourceField
	Deps        []string
	ReadyWhen   []string
	IncludeWhen []string
	GVR         schema.GroupVersionResource
}

func (m *MockResource) GetID() string                                        { return m.ID }
func (m *MockResource) Unstructured() *unstructured.Unstructured             { return m.Obj }
func (m *MockResource) GetVariables() []*variable.ResourceField              { return m.Vars }
func (m *MockResource) GetDependencies() []string                            { return m.Deps }
func (m *MockResource) GetReadyWhenExpressions() []string                    { return m.ReadyWhen }
func (m *MockResource) GetIncludeWhenExpressions() []string                  { return m.IncludeWhen }
func (m *MockResource) GetGroupVersionResource() schema.GroupVersionResource { return m.GVR }
func (m *MockResource) IsExternalRef() bool                                  { return false }
func (m *MockResource) IsNamespaced() bool                                   { return true }
func (m *MockResource) GetOrder() int                                        { return 0 }
func (m *MockResource) GetSchema() *spec.Schema                              { return nil }

func buildGraphOffline(rgd *v1alpha1.ResourceGraphDefinition, instanceObj *unstructured.Unstructured) (runtime.Resource, map[string]runtime.Resource, []string, error) {
	resources := make(map[string]runtime.Resource)
	var topoOrder []string
	resourceNames := []string{"schema"}

	for _, res := range rgd.Spec.Resources {
		topoOrder = append(topoOrder, res.ID)
		resourceNames = append(resourceNames, res.ID)
	}

	env, _ := krocel.DefaultEnvironment(krocel.WithResourceIDs(resourceNames))

	for _, rgRes := range rgd.Spec.Resources {
		var objMap map[string]interface{}
		if err := yaml.Unmarshal(rgRes.Template.Raw, &objMap); err != nil {
			return nil, nil, nil, fmt.Errorf("failed to parse template for %s: %w", rgRes.ID, err)
		}

		descriptors, err := parser.ParseSchemalessResource(objMap)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to parse variables for %s: %w", rgRes.ID, err)
		}

		var vars []*variable.ResourceField
		var allDeps []string

		for _, desc := range descriptors {
			inspector := ast.NewInspectorWithEnv(env, resourceNames)
			inspection, err := inspector.Inspect(desc.Expressions[0])
			if err != nil {
				return nil, nil, nil, fmt.Errorf("bad expression in %s: %w", rgRes.ID, err)
			}

			var deps []string
			kind := variable.ResourceVariableKindStatic
			for _, dep := range inspection.ResourceDependencies {
				if dep.ID != "schema" && dep.ID != rgRes.ID {
					deps = append(deps, dep.ID)
					if !slices.Contains(allDeps, dep.ID) {
						allDeps = append(allDeps, dep.ID)
					}
					kind = variable.ResourceVariableKindDynamic
				}
			}

			vars = append(vars, &variable.ResourceField{
				FieldDescriptor: desc,
				Kind:            kind,
				Dependencies:    deps,
			})
		}

		resources[rgRes.ID] = &MockResource{
			ID:          rgRes.ID,
			Obj:         &unstructured.Unstructured{Object: objMap},
			Vars:        vars,
			Deps:        allDeps,
			ReadyWhen:   rgRes.ReadyWhen,
			IncludeWhen: rgRes.IncludeWhen,
			GVR:         schema.GroupVersionResource{Group: "mock", Version: "v1", Resource: "mocks"},
		}
	}

	instance := &MockResource{
		ID:  "instance",
		Obj: instanceObj,
		GVR: schema.GroupVersionResource{Group: "mock", Version: "v1", Resource: "instances"},
	}

	return instance, resources, topoOrder, nil
}

func AddTestCommands(root *cobra.Command) {
	root.AddCommand(TestCmd)
}
