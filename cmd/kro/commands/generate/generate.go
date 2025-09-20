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
	"github.com/spf13/cobra"
)

type GenerateConfig struct {
	resourceGraphDefinitionFile string
	outputFormat                string
}

var generateConfig = &GenerateConfig{}

func init() {
	generateCmd.PersistentFlags().StringVarP(&generateConfig.resourceGraphDefinitionFile, "file", "f", "",
		"Path to the ResourceGraphDefinition file")
	generateCmd.PersistentFlags().StringVarP(&generateConfig.outputFormat, "format", "o",
		"yaml", "Output format (yaml|json)")
}

var generateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate the CRD from a given ResourceGraphDefinition",
	Long: "Generate the CRD from a given ResourceGraphDefinition." +
		"This command generates a CustomResourceDefinition (CRD) based on the provided ResourceGraphDefinition file.",
}

func AddGenerateCommands(rootCmd *cobra.Command) {
	generateCmd.AddCommand(generateCRDCmd)
	generateCmd.AddCommand(generateDiagramCmd)
	generateCmd.AddCommand(generateInstanceCmd)
	rootCmd.AddCommand(generateCmd)
}
