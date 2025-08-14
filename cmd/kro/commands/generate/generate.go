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

package generate

import (
	"encoding/json"
	"fmt"

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
