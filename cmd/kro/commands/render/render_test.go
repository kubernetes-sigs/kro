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

package render

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestRenderCommand(t *testing.T) {
	// 1. Setup Input Files
	rgdContent := `apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: test-rgd
spec:
  schema:
    apiVersion: v1alpha1
    kind: TestApp
    spec:
      appName: string
      replicas: number
  resources:
    - id: configmap
      template:
        apiVersion: "v1"
        kind: "ConfigMap"
        metadata:
          name: "${schema.spec.appName}-config"
        data:
          app: "${schema.spec.appName}"
          replicas: "${schema.spec.replicas}"
`
	instanceContent := `apiVersion: kro.run/v1alpha1
kind: TestApp
metadata:
  name: my-test-app
spec:
  appName: "myapp"
  replicas: 3
`
	rgdFile, err := os.CreateTemp("", "rgd-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp RGD file: %v", err)
	}
	defer os.Remove(rgdFile.Name())

	if _, err := rgdFile.WriteString(rgdContent); err != nil {
		t.Fatalf("Failed to write RGD content: %v", err)
	}
	rgdFile.Close()

	instanceFile, err := os.CreateTemp("", "instance-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp instance file: %v", err)
	}
	defer os.Remove(instanceFile.Name())

	if _, err := instanceFile.WriteString(instanceContent); err != nil {
		t.Fatalf("Failed to write instance content: %v", err)
	}
	instanceFile.Close()

	// 2. Run Command
	cmd := NewRenderCommand()
	cmd.SetArgs([]string{"-f", rgdFile.Name(), "-i", instanceFile.Name()})

	// Capture Output
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err = cmd.Execute()
	if err != nil {
		t.Fatalf("Command failed: %v\nOutput: %s", err, buf.String())
	}

	output := buf.String()

	// 3. Verify Output
	if !strings.Contains(output, "myapp-config") {
		t.Errorf("Expected rendered name 'myapp-config' not found in output:\n%s", output)
	}

	if !strings.Contains(output, "app: myapp") {
		t.Errorf("Expected 'app: myapp' not found in output:\n%s", output)
	}

	// Check for replicas (could be string or number in YAML)
	if !strings.Contains(output, "replicas: \"3\"") && !strings.Contains(output, "replicas: 3") {
		t.Errorf("Expected 'replicas: 3' not found in output:\n%s", output)
	}
}
