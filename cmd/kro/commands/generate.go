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

package commands

import (
	"fmt"
	"os"
	"strings"

	"github.com/kro-run/kro/api/v1alpha1"
	kroclient "github.com/kro-run/kro/pkg/client"
	"github.com/kro-run/kro/pkg/graph"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

type GenerateConfig struct {
	resourceGroupDefinitionFile string
}

var config = &GenerateConfig{}

func init() {
	generateCmd.PersistentFlags().StringVarP(&config.resourceGroupDefinitionFile, "file", "f", "",
		"Path to the ResourceGroupDefinition file")
}

var generateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate the CRD from a given ResourceGroupDefinition",
	Long: "Generate the CRD from a given ResourceGroupDefinition." +
		"This command generates a CustomResourceDefinition (CRD) based on the provided ResourceGroupDefinition file.",
}

var generateDiagramCmd = &cobra.Command{
	Use:   "diagram",
	Short: "Generate a diagram from a ResourceGroupDefinition",
	Long: "Generate a diagram from a ResourceGroupDefinition file. This command reads the " +
		"ResourceGroupDefinition and outputs the corresponding diagram " +
		"in the specified format.",
	RunE: func(cmd *cobra.Command, args []string) error {
		if config.resourceGroupDefinitionFile == "" {
			return fmt.Errorf("ResourceGroupDefinition file is required")
		}

		data, err := os.ReadFile(config.resourceGroupDefinitionFile)
		if err != nil {
			return fmt.Errorf("failed to read ResourceGroupDefinition file: %w", err)
		}

		var rgd v1alpha1.ResourceGraphDefinition
		if err = yaml.Unmarshal(data, &rgd); err != nil {
			return fmt.Errorf("failed to unmarshal ResourceGroupDefinition: %w", err)
		}

		if err = generateDiagram(&rgd); err != nil {
			return fmt.Errorf("failed to generate diagram: %w", err)
		}

		return nil
	},
}

func generateDiagram(rgd *v1alpha1.ResourceGraphDefinition) error {
	rgdGraph, err := createGraphBuilder(rgd)
	if err != nil {
		return fmt.Errorf("failed to setup rgd graph: %w", err)
	}

	fmt.Println("graph LR")
	fmt.Println("    %% Set viewport and styling")
	fmt.Println("    %% This helps with zoom and spacing")

	allNodes := make(map[string]string)
	hasEdges := false

	for resourceName, vertex := range rgdGraph.DAG.Vertices {
		cleanName := strings.ReplaceAll(resourceName, "-", "_")
		cleanName = strings.ReplaceAll(cleanName, ".", "_")
		allNodes[cleanName] = resourceName

		for dep := range vertex.DependsOn {
			cleanDep := strings.ReplaceAll(dep, "-", "_")
			cleanDep = strings.ReplaceAll(cleanDep, ".", "_")
			allNodes[cleanDep] = dep

			fmt.Printf("    %s --> %s\n", cleanDep, cleanName)
			hasEdges = true
		}
	}

	if !hasEdges && len(allNodes) > 0 {
		for cleanName, originalName := range allNodes {
			fmt.Printf("    %s[\"%s\"]\n", cleanName, originalName)
		}
	}

	if len(allNodes) == 0 {
		fmt.Println("    EmptyGraph[\"No resources found\"]")
	}

	fmt.Println("    classDef default fill:#e1f5fe,stroke:#0277bd,stroke-width:2px")

	return nil
}

func createGraphBuilder(rgd *v1alpha1.ResourceGraphDefinition) (*graph.Graph, error) {
	set, err := kroclient.NewSet(kroclient.Config{})
	if err != nil {
		return nil, fmt.Errorf("failed to create client set: %w", err)
	}

	restConfig := set.RESTConfig()

	builder, err := graph.NewBuilder(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create graph builder: %w", err)
	}

	rgdGraph, err := builder.NewResourceGraphDefinition(rgd)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource graph definition: %w", err)
	}

	return rgdGraph, nil
}

func AddGenerateCommands(rootCmd *cobra.Command) {
	generateCmd.AddCommand(generateDiagramCmd)
	rootCmd.AddCommand(generateCmd)
}
