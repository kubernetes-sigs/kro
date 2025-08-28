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
	"path/filepath"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/kro-run/kro/api/v1alpha1"
)

type PackageConfig struct {
	resourceGraphDefinitionFile string
	tag                         string
}

var packageConfig = &PackageConfig{}

func init() {
	packageRGDCmd.PersistentFlags().StringVarP(&packageConfig.resourceGraphDefinitionFile,
		"file", "f", "",
		"Path to the ResourceGraphDefinition file",
	)

	packageRGDCmd.PersistentFlags().StringVarP(&packageConfig.tag,
		"tag", "t", "latest",
		"Tag to use for the OCI image (default: latest)")
}

var packageRGDCmd = &cobra.Command{
	Use:   "package",
	Short: "Create an OCI Image packaging the ResourceGraphDefinition",
	Long: "Package command packages the ResourceGraphDefinition" +
		"file into an OCI image, which can be used for distribution and deployment.",
	RunE: func(cmd *cobra.Command, args []string) error {
		if packageConfig.resourceGraphDefinitionFile == "" {
			return fmt.Errorf("ResourceGraphDefinition file is required")
		}

		data, err := os.ReadFile(packageConfig.resourceGraphDefinitionFile)
		if err != nil {
			return fmt.Errorf("failed to read ResourceGraphDefinition file: %w", err)
		}

		var rgd v1alpha1.ResourceGraphDefinition
		if err := yaml.Unmarshal(data, &rgd); err != nil {
			return fmt.Errorf("failed to unmarshal ResourceGraphDefinition: %w", err)
		}

		basename := filepath.Base(packageConfig.resourceGraphDefinitionFile)
		extension := filepath.Ext(basename)
		nameWithoutExt := basename[:len(basename)-len(extension)]
		outputFile := nameWithoutExt + ".tar"

		if err = packageRGD(outputFile, data, &rgd); err != nil {
			return fmt.Errorf("failed to package ResourceGraphDefinition: %w", err)
		}

		fmt.Println("Successfully packaged ResourceGraphDefinition to", outputFile)

		return nil
	},
}

func packageRGD(outputFile string, data []byte, rgd *v1alpha1.ResourceGraphDefinition) error {
	layer := static.NewLayer(data, types.MediaType("application/vnd.kro.resourcegraphdefinition.v1alpha1+yaml"))

	img := empty.Image

	img, err := mutate.AppendLayers(img, layer)

	if err != nil {
		return fmt.Errorf("failed to append layer: %w", err)
	}

	configFile, err := img.ConfigFile()
	if err != nil {
		return fmt.Errorf("failed to get config file: %w", err)
	}

	now := time.Now()
	configFile.Created = v1.Time{Time: now}

	configFile.Config.Labels = map[string]string{
		"kro.run/type": "resourcegraphdefinition",
		"kro.run/name": rgd.Name,
	}

	img, err = mutate.ConfigFile(img, configFile)
	if err != nil {
		return fmt.Errorf("failed to update image config: %w", err)
	}

	ref, err := name.ParseReference(fmt.Sprintf("kro.run/rgd/%s:%s", rgd.Name, packageConfig.tag))
	if err != nil {
		return fmt.Errorf("failed to parse image reference: %w", err)
	}

	if err := tarball.WriteToFile(outputFile, ref, img); err != nil {
		return fmt.Errorf("failed to write image to file: %w", err)
	}

	return nil
}

func AddPackageCommand(rootCmd *cobra.Command) {
	rootCmd.AddCommand(packageRGDCmd)
}
