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
	"encoding/json"
	"fmt"
	"os"

	"github.com/go-echarts/go-echarts/v2/charts"
	"github.com/go-echarts/go-echarts/v2/opts"
	"github.com/kro-run/kro/api/v1alpha1"
	kroclient "github.com/kro-run/kro/pkg/client"
	"github.com/kro-run/kro/pkg/graph"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

type GenerateConfig struct {
	resourceGraphDefinitionFile string
	outputFormat                string
}

var config = &GenerateConfig{}

func init() {
	generateCmd.PersistentFlags().StringVarP(&config.resourceGraphDefinitionFile, "file", "f", "",
		"Path to the ResourceGraphDefinition file")
	generateCmd.PersistentFlags().StringVarP(&config.outputFormat, "format", "o", "yaml", "Output format (yaml|json)")
}

var generateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate the CRD from a given ResourceGraphDefinition",
	Long: "Generate the CRD from a given ResourceGraphDefinition." +
		"This command generates a CustomResourceDefinition (CRD) based on the provided ResourceGraphDefinition file.",
}

var generateCRDCmd = &cobra.Command{
	Use:   "crd",
	Short: "Generate a CustomResourceDefinition (CRD)",
	Long: "Generate a CustomResourceDefinition (CRD) from a " +
		"ResourceGraphDefinition file. This command reads the " +
		"ResourceGraphDefinition and outputs the corresponding CRD " +
		"in the specified format.",
	RunE: func(cmd *cobra.Command, args []string) error {
		if config.resourceGraphDefinitionFile == "" {
			return fmt.Errorf("ResourceGraphDefinition file is required")
		}

		data, err := os.ReadFile(config.resourceGraphDefinitionFile)
		if err != nil {
			return fmt.Errorf("failed to read ResourceGraphDefinition file: %w", err)
		}

		var rgd v1alpha1.ResourceGraphDefinition
		if err = yaml.Unmarshal(data, &rgd); err != nil {
			return fmt.Errorf("failed to unmarshal ResourceGraphDefinition: %w", err)
		}

		if err = generateCRD(&rgd); err != nil {
			return fmt.Errorf("failed to generate CRD: %w", err)
		}

		return nil
	},
}

var generateInstanceCmd = &cobra.Command{
	Use:   "instance",
	Short: "Generate an instance of the ResourceGraphDefinition",
	Long: "Generate a ResourceGraphDefinition (RGD) instance from a " +
		"ResourceGraphDefinition file. This command reads the " +
		"ResourceGraphDefinition and outputs the corresponding RGD instance",
	RunE: func(cmd *cobra.Command, args []string) error {
		if config.resourceGraphDefinitionFile == "" {
			return fmt.Errorf("ResourceGraphDefinition file is required")
		}

		data, err := os.ReadFile(config.resourceGraphDefinitionFile)
		if err != nil {
			return fmt.Errorf("failed to read ResourceGraphDefinition file: %w", err)
		}

		var rgd v1alpha1.ResourceGraphDefinition
		if err = yaml.Unmarshal(data, &rgd); err != nil {
			return fmt.Errorf("failed to unmarshal ResourceGraphDefinition: %w", err)
		}

		if err = generateInstance(&rgd); err != nil {
			return fmt.Errorf("failed to generate instance: %w", err)
		}

		return nil
	},
}

var generateDiagramCmd = &cobra.Command{
	Use:   "diagram",
	Short: "Generate a diagram from a ResourceGraphDefinition",
	Long: "Generate a diagram from a ResourceGraphDefinition file. This command reads the " +
		"ResourceGraphDefinition and outputs the corresponding diagram " +
		"in the specified format.",
	RunE: func(cmd *cobra.Command, args []string) error {
		if config.resourceGraphDefinitionFile == "" {
			return fmt.Errorf("ResourceGraphDefinition file is required")
		}

		data, err := os.ReadFile(config.resourceGraphDefinitionFile)
		if err != nil {
			return fmt.Errorf("failed to read ResourceGraphDefinition file: %w", err)
		}

		var rgd v1alpha1.ResourceGraphDefinition
		if err = yaml.Unmarshal(data, &rgd); err != nil {
			return fmt.Errorf("failed to unmarshal ResourceGraphDefinition: %w", err)
		}

		if err = generateDiagram(&rgd); err != nil {
			return fmt.Errorf("failed to generate diagram: %w", err)
		}

		return nil
	},
}

func generateCRD(rgd *v1alpha1.ResourceGraphDefinition) error {
	rgdGraph, err := createGraphBuilder(rgd)
	if err != nil {
		return fmt.Errorf("failed to setup rgd graph: %w", err)
	}

	crd := rgdGraph.Instance.GetCRD()
	crd.SetAnnotations(map[string]string{"kro.run/version": "dev"})

	b, err := marshalObject(crd, config.outputFormat)
	if err != nil {
		return fmt.Errorf("failed to marshal CRD: %w", err)
	}

	fmt.Println(string(b))

	return nil
}

func generateDiagram(rgd *v1alpha1.ResourceGraphDefinition) error {
	rgdGraph, err := createGraphBuilder(rgd)
	if err != nil {
		return fmt.Errorf("failed to setup rgd graph: %w", err)
	}

	graph := charts.NewGraph()

	// Graph layout
	graph.SetGlobalOptions(
		charts.WithInitializationOpts(opts.Initialization{
			Width:  "70%",
			Height: "90vh",
		}),
		charts.WithTitleOpts(opts.Title{
			Title:    fmt.Sprintf("Resource Graph: %s", rgd.Name),
			Subtitle: fmt.Sprintf("Topological Order: %v", rgdGraph.TopologicalOrder),
			TitleStyle: &opts.TextStyle{
				FontSize:   18,
				FontWeight: "bold",
			},
			SubtitleStyle: &opts.TextStyle{
				FontSize: 14,
			},
		}),
		charts.WithLegendOpts(opts.Legend{
			Show: opts.Bool(false),
		}),
	)

	resourceOrder := make(map[string]int)
	for i, resourceName := range rgdGraph.TopologicalOrder {
		resourceOrder[resourceName] = i + 1
	}

	// Graph nodes
	nodes := make([]opts.GraphNode, 0, len(rgdGraph.Resources))
	for resourceName := range rgdGraph.Resources {
		order := resourceOrder[resourceName]

		node := opts.GraphNode{
			Name:       fmt.Sprintf("%s\n(%d)", resourceName, order),
			SymbolSize: 40.0,
			ItemStyle: &opts.ItemStyle{
				Color:       "#269103",
				BorderColor: "#333",
				BorderWidth: 1,
			},
		}
		nodes = append(nodes, node)
	}

	// Graph links
	links := make([]opts.GraphLink, 0)
	for resourceName, resource := range rgdGraph.Resources {
		targetOrder := resourceOrder[resourceName]

		for _, dependency := range resource.GetDependencies() {
			sourceOrder := resourceOrder[dependency]

			link := opts.GraphLink{
				Source: fmt.Sprintf("%s\n(%d)", dependency, sourceOrder),
				Target: fmt.Sprintf("%s\n(%d)", resourceName, targetOrder),
				LineStyle: &opts.LineStyle{
					Color: "#000",
				},
			}
			links = append(links, link)
		}
	}

	graph.AddSeries("resource", nodes, links).SetSeriesOptions(
		charts.WithGraphChartOpts(
			opts.GraphChart{
				Force: &opts.GraphForce{
					Repulsion: 5000,
				},
				Roam:           opts.Bool(true),
				EdgeSymbol:     "arrow",
				EdgeSymbolSize: []int{0, 7},
			},
		),
	)

	return graph.Render(os.Stdout)
}

func generateInstance(rgd *v1alpha1.ResourceGraphDefinition) error {
	rgdGraph, err := createGraphBuilder(rgd)
	if err != nil {
		return fmt.Errorf("failed to create resource graph definition: %w", err)
	}

	emulatedObj := rgdGraph.Instance.GetEmulatedObject()
	emulatedObj.SetAnnotations(map[string]string{"kro.run/version": "dev"})

	delete(emulatedObj.Object, "status")

	b, err := marshalObject(emulatedObj, config.outputFormat)
	if err != nil {
		return fmt.Errorf("failed to marshal CRD: %w", err)
	}

	fmt.Println(string(b))

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

func marshalObject(obj interface{}, outputFormat string) ([]byte, error) {
	var b []byte
	var err error

	if obj == nil {
		return nil, fmt.Errorf("nil object provided for marshaling")
	}

	switch outputFormat {
	case "json":
		b, err = json.Marshal(obj)
		if err != nil {
			return nil, err
		}
	case "yaml":
		b, err = yaml.Marshal(obj)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported output format: %s", outputFormat)
	}

	return b, nil
}

func AddGenerateCommands(rootCmd *cobra.Command) {
	generateCmd.AddCommand(generateCRDCmd)
	generateCmd.AddCommand(generateDiagramCmd)
	generateCmd.AddCommand(generateInstanceCmd)
	rootCmd.AddCommand(generateCmd)
}
