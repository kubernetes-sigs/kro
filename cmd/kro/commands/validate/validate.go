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

package validate

import (
	"fmt"
	"os"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	kroclient "github.com/kubernetes-sigs/kro/pkg/client"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate the ResourceGraphDefinition",
	Long: `Validate the ResourceGraphDefinition. This command checks ` +
		`if the ResourceGraphDefinition is valid and can be used to create a ResourceGraph.`,
}

var (
	resourceGroupDefinitionFile string
	kubernetesVersion           string
	crdsDir                     string
	fromCluster                 bool
)

// defaultRGDConfig is the default RGD configuration for CLI validation
var defaultRGDConfig = graph.RGDConfig{
	MaxCollectionSize:          1000,
	MaxCollectionDimensionSize: 10,
}

func init() {
	validateRGDCmd.PersistentFlags().StringVarP(&resourceGroupDefinitionFile, "file", "f", "",
		"Path to the ResourceGroupDefinition file")
	validateRGDCmd.PersistentFlags().StringVar(&kubernetesVersion, "kubernetes-version", "",
		"Kubernetes version for embedded schemas (e.g., v1.30). Uses latest if not specified.")
	validateRGDCmd.PersistentFlags().StringVar(&crdsDir, "crds", "",
		"Directory containing local CRD files for schema resolution")
	validateRGDCmd.PersistentFlags().BoolVar(&fromCluster, "from-cluster", false,
		"Discover schemas from Kubernetes API server (requires kubeconfig)")
}

var validateRGDCmd = &cobra.Command{
	Use:   "rgd [FILE]",
	Short: "Validate a ResourceGraphDefinition file",
	Long: `Validate a ResourceGraphDefinition file using different schema resolution strategies:

  Offline (default): Uses embedded Kubernetes schemas for validation
  --crds <dir>: Include local CRD files (extends embedded schemas)
  --kubernetes-version <version>: Specify Kubernetes version (defaults to latest)
  --from-cluster: Validate against live cluster API server

Offline validation is useful for CI/CD pipelines and pre-deployment validation without
requiring cluster access. Use --from-cluster when you need to validate against cluster-specific
CRDs or alpha/beta APIs that may differ between Kubernetes versions.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if resourceGroupDefinitionFile == "" {
			return fmt.Errorf("ResourceGroupDefinition file is required")
		}

		data, err := os.ReadFile(resourceGroupDefinitionFile)
		if err != nil {
			return fmt.Errorf("reading ResourceGroupDefinition file: %w", err)
		}

		var rgd v1alpha1.ResourceGraphDefinition
		if err = yaml.Unmarshal(data, &rgd); err != nil {
			return fmt.Errorf("unmarshaling ResourceGroupDefinition: %w", err)
		}

		if err := validateRGD(&rgd); err != nil {
			return fmt.Errorf("validation failed: %w", err)
		}

		fmt.Println("Validation successful! The ResourceGraphDefinition is valid.")
		return nil
	},
}

func validateRGD(rgd *v1alpha1.ResourceGraphDefinition) error {
	var builder *graph.Builder
	var err error

	if fromCluster {
		// Cluster validation - use live API server
		set, err := kroclient.NewSet(kroclient.Config{})
		if err != nil {
			return fmt.Errorf("creating client set: %w", err)
		}

		builder, err = graph.NewBuilder(set.RESTConfig(), set.HTTPClient())
		if err != nil {
			return fmt.Errorf("creating graph builder: %w", err)
		}
	} else {
		// Offline validation - use single resolver for both schemas and scopes
		resolver, err := NewOfflineSchemaResolver(kubernetesVersion, crdsDir)
		if err != nil {
			return fmt.Errorf("creating offline resolver: %w", err)
		}

		builder = graph.NewBuilderWithResolver(resolver, resolver)
	}

	_, err = builder.NewResourceGraphDefinition(rgd, defaultRGDConfig)
	if err != nil {
		return fmt.Errorf("failed to create ResourceGraphDefinition: %w", err)
	}

	return nil
}

func AddValidateCommands(rootCmd *cobra.Command) {
	validateCmd.AddCommand(validateRGDCmd)
	rootCmd.AddCommand(validateCmd)
}
