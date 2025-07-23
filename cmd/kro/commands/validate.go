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

	"github.com/kro-run/kro/api/v1alpha1"
	kroclient "github.com/kro-run/kro/pkg/client"
	"github.com/kro-run/kro/pkg/graph"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate the ResourceGraphDefinition",
	Long: `Validate the ResourceGraphDefinition. This command checks ` +
		`if the ResourceGraphDefinition is valid and can be used to create a ResourceGraph.`,
}

var resourceGroupDefinitionFile string

func init() {
	validateRGDCmd.PersistentFlags().StringVarP(&resourceGroupDefinitionFile, "file", "f", "",
		"Path to the ResourceGroupDefinition file")
}

var validateRGDCmd = &cobra.Command{
	Use:   "rgd [FILE]",
	Short: "Validate a ResourceGraphDefinition file",
	RunE: func(cmd *cobra.Command, args []string) error {
		if resourceGroupDefinitionFile == "" {
			return fmt.Errorf("ResourceGroupDefinition file is required")
		}

		data, err := os.ReadFile(resourceGroupDefinitionFile)
		if err != nil {
			return fmt.Errorf("failed to read ResourceGroupDefinition file: %w", err)
		}

		var rgd v1alpha1.ResourceGraphDefinition
		if err = yaml.Unmarshal(data, &rgd); err != nil {
			return fmt.Errorf("failed to unmarshal ResourceGroupDefinition: %w", err)
		}

		if err := validateRGD(&rgd); err != nil {
			return fmt.Errorf("validation failed: %w", err)
		}

		fmt.Println("Validation successful! The ResourceGraphDefinition is valid.")
		return nil
	},
}

func validateRGD(rgd *v1alpha1.ResourceGraphDefinition) error {
	set, err := kroclient.NewSet(kroclient.Config{})
	if err != nil {
		return fmt.Errorf("failed to create client set: %w", err)
	}

	restConfig := set.RESTConfig()

	builder, err := graph.NewBuilder(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create graph builder: %w", err)
	}

	_, err = builder.NewResourceGraphDefinition(rgd)
	if err != nil {
		return fmt.Errorf("failed to create ResourceGraphDefinition: %w", err)
	}

	return nil
}

func AddValidateCommands(rootCmd *cobra.Command) {
	validateCmd.AddCommand(validateRGDCmd)
	rootCmd.AddCommand(validateCmd)
}
