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

package generate

import (
	"fmt"
	"os"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

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
