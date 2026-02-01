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
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// RenderConfig holds configuration for the render command
type RenderConfig struct {
	resourceGraphDefinitionFile string
	outputFormat                string
	instanceFile                string
}

var config = &RenderConfig{}

var renderCmd = &cobra.Command{
	Use:   "render",
	Short: "Render RGD templates with instance details offline",
	Long: `Generates Kubernetes resources by combining a ResourceGraphDefinition (RGD) with an Instance file.
    
This command runs purely offline without requiring a Kubernetes cluster connection.
It reads an RGD file and an instance file, then renders the fully hydrated resources
by substituting variables (${schema.spec.x}) with values from the instance.`,
	Example: `  # Render to stdout (YAML)
  kro generate render -f my-rgd.yaml -i my-instance.yaml

  # Render to JSON
  kro generate render -f my-rgd.yaml -i my-instance.yaml -o json

  # Render using stdin
  cat my-instance.yaml | kro generate render -f my-rgd.yaml -i -
  
  # Pipe from generate instance
  kro generate instance -f my-rgd.yaml | kro generate render -f my-rgd.yaml -i -`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRender(cmd)
	},
}

func init() {
	// Register flags and bind them to the config struct
	renderCmd.Flags().StringVarP(&config.resourceGraphDefinitionFile, "file", "f", "", "Path to the ResourceGraphDefinition file")
	renderCmd.Flags().StringVarP(&config.instanceFile, "instance", "i", "", "Path to the Instance file (required, use '-' for stdin)")
	renderCmd.Flags().StringVarP(&config.outputFormat, "format", "o", "yaml", "Output format (yaml|json)")

	// Mark required flags
	_ = renderCmd.MarkFlagRequired("instance")
	_ = renderCmd.MarkFlagRequired("file")
}

// NewRenderCommand returns the render command for registration with parent commands
func NewRenderCommand() *cobra.Command {
	return renderCmd
}

func runRender(cmd *cobra.Command) error {
	// 1. Read and Parse RGD
	if config.resourceGraphDefinitionFile == "" {
		return fmt.Errorf("ResourceGraphDefinition file is required (use -f flag)")
	}

	rgdData, err := os.ReadFile(config.resourceGraphDefinitionFile)
	if err != nil {
		return fmt.Errorf("failed to read RGD file: %w", err)
	}

	var rgd v1alpha1.ResourceGraphDefinition
	if err := yaml.Unmarshal(rgdData, &rgd); err != nil {
		return fmt.Errorf("failed to parse RGD YAML: %w", err)
	}

	// 2. Read and Parse Instance
	var instanceData []byte
	if config.instanceFile == "-" {
		instanceData, err = io.ReadAll(cmd.InOrStdin()) // Use cobra's input stream for better testing
		if err != nil {
			return fmt.Errorf("failed to read instance from stdin: %w", err)
		}
	} else {
		instanceData, err = os.ReadFile(config.instanceFile)
		if err != nil {
			return fmt.Errorf("failed to read instance file: %w", err)
		}
	}

	var instance unstructured.Unstructured
	if err := yaml.Unmarshal(instanceData, &instance); err != nil {
		return fmt.Errorf("failed to parse Instance YAML: %w", err)
	}

	// 3. Build Graph (Now using local function, no pkg/graph import needed)
	g, err := BuildGraphOffline(&rgd, &instance)
	if err != nil {
		return fmt.Errorf("rendering failed: %w", err)
	}

	// 4. Output Results
	// Use cmd.OutOrStdout() to ensure output is captured by tests
	out := cmd.OutOrStdout()

	for _, resourceID := range g.Order {
		resource := g.Resources[resourceID]
		if resource == nil {
			continue
		}

		var output []byte
		if config.outputFormat == "json" {
			output, err = json.MarshalIndent(resource.Object, "", "  ")
		} else {
			output, err = yaml.Marshal(resource.Object)
		}

		if err != nil {
			return fmt.Errorf("failed to marshal output for %s: %w", resourceID, err)
		}

		if config.outputFormat == "yaml" {
			fmt.Fprintln(out, "---")
		}
		fmt.Fprintln(out, string(output))
	}

	return nil
}
