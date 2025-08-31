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

package pkg

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"

	"github.com/kro-run/kro/api/v1alpha1"
)

type PackageConfig struct {
	resourceGraphDefinitionFile string
	tag                         string
}

var packageConfig = &PackageConfig{}

func init() {
	packageRGDCmd.PersistentFlags().StringVarP(&packageConfig.resourceGraphDefinitionFile,
		"file", "f", "rgd.yaml",
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
		"file into an OCI image bundle, which can be used for distribution and deployment.",
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
		ext := filepath.Ext(basename)
		nameWithoutExt := basename[:len(basename)-len(ext)]
		outputTar := nameWithoutExt + ".tar"

		if err := packageRGD(outputTar, data, &rgd); err != nil {
			return fmt.Errorf("failed to package ResourceGraphDefinition: %w", err)
		}

		fmt.Println("Successfully packaged ResourceGraphDefinition to", outputTar)
		return nil
	},
}

func packageRGD(outputTar string, data []byte, rgd *v1alpha1.ResourceGraphDefinition) error {
	ctx := context.Background()

	tempDir, err := os.MkdirTemp("", "kro-oci-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			fmt.Println("failed to remove temp dir", tempDir, ":", err)
		}
	}()

	store, err := oci.New(tempDir)
	if err != nil {
		return fmt.Errorf("failed to create OCI layout store: %w", err)
	}

	mediaType := "application/vnd.kro.resourcegraphdefinition"
	blobDesc, err := oras.PushBytes(ctx, store, mediaType, data)
	if err != nil {
		return fmt.Errorf("failed to push RGD blob: %w", err)
	}

	layer := blobDesc
	if layer.Annotations == nil {
		layer.Annotations = map[string]string{}
	}
	layer.Annotations[ocispec.AnnotationTitle] = filepath.Base(packageConfig.resourceGraphDefinitionFile)

	artifactType := "application/vnd.kro.resourcegraphdefinition"
	packOpts := oras.PackManifestOptions{
		Layers: []ocispec.Descriptor{layer},
		ManifestAnnotations: map[string]string{
			"kro.run/type": "resourcegraphdefinition",
			"kro.run/name": rgd.Name,
		},
	}

	manifestDesc, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, artifactType, packOpts)
	if err != nil {
		return fmt.Errorf("failed to pack manifest: %w", err)
	}

	tag := fmt.Sprintf("%s:%s", rgd.Name, packageConfig.tag)
	if err := store.Tag(ctx, manifestDesc, tag); err != nil {
		return fmt.Errorf("failed to tag manifest in layout: %w", err)
	}

	if err := os.RemoveAll(filepath.Join(tempDir, "ingest")); err != nil {
		fmt.Println("failed to remove ingest folder: ", err)
	}

	if err := tarDir(tempDir, outputTar); err != nil {
		return fmt.Errorf("failed to write OCI layout tar: %w", err)
	}

	return nil
}

func tarDir(srcDir, tarPath string) error {
	out, err := os.Create(tarPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := out.Close(); err != nil {
			fmt.Println("failed to close tar file: ", err)
		}
	}()

	tw := tar.NewWriter(out)
	defer func() {
		if err := tw.Close(); err != nil {
			fmt.Println("failed to close tar writer: ", err)
		}
	}()

	return filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || path == srcDir {
			return err
		}

		info, _ := d.Info()
		rel, _ := filepath.Rel(srcDir, path)
		rel = filepath.ToSlash(rel)

		switch {
		case strings.HasPrefix(rel, "ingest/"),
			strings.HasPrefix(rel, "blobs/sha256/") && info.Size() == 0,
			filepath.Clean(path) == filepath.Clean(tarPath):
			return nil
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}

		if !info.Mode().IsRegular() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() {
			if err := f.Close(); err != nil {
				fmt.Println("failed to close file ", path, ":", err)
			}
		}()

		_, err = io.Copy(tw, f)
		return err
	})
}

func AddPackageCommand(rootCmd *cobra.Command) {
	rootCmd.AddCommand(packageRGDCmd)
}
