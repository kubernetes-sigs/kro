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
	"errors"
	"fmt"
	"os"
	"regexp"
	"sigs.k8s.io/yaml"

	kroapi "github.com/kro-run/kro/api/v1alpha1"
	"github.com/spf13/cobra"
)

var (
	kindRegex = regexp.MustCompile(`^[A-Z][a-zA-Z0-9]{0,62}$`)
	apiRegex  = regexp.MustCompile(`^v[0-9]+(alpha[0-9]+|beta[0-9]+)?$`)
)

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate the ResourceGraphDefinition",
	Long: `Validate the ResourceGraphDefinition. This command checks 
	if the ResourceGraphDefinition is valid and can be used to create a ResourceGraph.`,
}

var validateRGDCmd = &cobra.Command{
	Use:   "rgd [FILE]",
	Short: "Validate a ResourceGraphDefinition file",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path := args[0]

		// call validateRGDCmd function
		if err := ValidateRGDFile(path); err != nil {
			return fmt.Errorf("validation failed: %w", err)
		}

		fmt.Println("ResourceGraphDefinition is valid")
		return nil
	},
}

func ValidateRGDFile(path string) error {
	// Read file content
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read file: %w", err)
	}

	// Strictly unmarshal into the Kro API type
	var rgd kroapi.ResourceGraphDefinition
	if err := yaml.UnmarshalStrict(data, &rgd); err != nil {
		return fmt.Errorf("invalid YAML: %w", err)
	}

	// 1. Ensure spec.schema is present
	if rgd.Spec.Schema == nil {
		return errors.New("spec.schema is required")
	}
	// 2. Validate schema Kind
	if !kindRegex.MatchString(rgd.Spec.Schema.Kind) {
		return fmt.Errorf("schema.kind invalid: %s", rgd.Spec.Schema.Kind)
	}
	// 3. Validate schema APIVersion
	if !apiRegex.MatchString(rgd.Spec.Schema.APIVersion) {
		return fmt.Errorf("schema.apiVersion invalid: %s", rgd.Spec.Schema.APIVersion)
	}

	// Pre-allocate map for resource IDs
	resources := rgd.Spec.Resources
	ids := make(map[string]struct{}, len(resources))

	// Phase 1: collect IDs and check for empty or duplicates
	for _, res := range resources {
		if res.ID == "" {
			return errors.New("every resource must have a non-empty id")
		}
		if _, found := ids[res.ID]; found {
			return fmt.Errorf("duplicate resource id: %s", res.ID)
		}
		ids[res.ID] = struct{}{}
	}

	// Phase 2: validate templates and dependencies
	for _, res := range resources {
		// Verify template is valid YAML or JSON
		var tmp any
		if err := yaml.Unmarshal(res.Template.Raw, &tmp); err != nil {
			return fmt.Errorf("invalid template for resource %s: %w", res.ID, err)
		}
		// Ensure ReadyWhen and IncludeWhen refer only to known IDs
		for _, dep := range append(res.ReadyWhen, res.IncludeWhen...) {
			if _, ok := ids[dep]; !ok {
				return fmt.Errorf(
					"resource %s references unknown dependency id %s",
					res.ID, dep,
				)
			}
		}
	}

	return nil
}

func AddValidateCommands(rootCmd *cobra.Command) {
	validateCmd.AddCommand(validateRGDCmd)
	rootCmd.AddCommand(validateCmd)
}
