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

package publish

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

type PublishConfig struct {
	ociTarballPath string
	remoteRef      string
	username       string
	password       string
}

var publishConfig = &PublishConfig{}

func init() {
	publishCmd.PersistentFlags().StringVarP(&publishConfig.ociTarballPath,
		"file", "f", "",
		"Path to the OCI image tarball created by the 'package' command",
	)
	publishCmd.PersistentFlags().StringVarP(&publishConfig.remoteRef,
		"ref", "r", "",
		"Remote reference to publish the OCI image to (e.g., 'ghcr.io/user/repo:tag')",
	)
	publishCmd.PersistentFlags().StringVarP(&publishConfig.username,
		"username", "u", "",
		"Username for the remote registry",
	)
	publishCmd.PersistentFlags().StringVarP(&publishConfig.password,
		"password", "p", "",
		"Password for the remote registry",
	)
}

var publishCmd = &cobra.Command{
	Use:   "publish",
	Short: "Publish a packaged OCI image to a remote registry",
	Long: "The publish command takes an OCI image tarball, created by the 'package' command, " +
		"and pushes it to a specified container registry.",
	RunE: func(cmd *cobra.Command, args []string) error {
		if publishConfig.ociTarballPath == "" {
			return fmt.Errorf("path to the OCI tarball is required, please use the --file flag")
		}

		if publishConfig.remoteRef == "" {
			return fmt.Errorf("remote reference is required, please use the --ref flag")
		}

		if publishConfig.username == "" {
			fmt.Print("Username: ")
			reader := bufio.NewReader(os.Stdin)
			username, err := reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("failed to read username: %w", err)
			}
			publishConfig.username = strings.TrimSpace(username)
		}

		if publishConfig.password == "" {
			fmt.Print("Password: ")
			bytePassword, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Println()
			if err != nil {
				return fmt.Errorf("failed to read password: %w", err)
			}
			publishConfig.password = string(bytePassword)
		}

		ctx := context.Background()
		repo, err := remote.NewRepository(publishConfig.remoteRef)
		if err != nil {
			return fmt.Errorf("failed to create remote repository for %s: %w", publishConfig.remoteRef, err)
		}

		credentialFunc := func(ctx context.Context, registry string) (auth.Credential, error) {
			return auth.Credential{
				Username: publishConfig.username,
				Password: publishConfig.password,
			}, nil
		}

		repo.Client = &auth.Client{
			Client:     retry.DefaultClient,
			Cache:      auth.NewCache(),
			Credential: credentialFunc,
		}

		if publishConfig.username == "" || publishConfig.password == "" {
			return fmt.Errorf("username and password are required")
		}

		tempDir, err := os.MkdirTemp("", "kro-publish-*")
		if err != nil {
			return fmt.Errorf("failed to create temp dir: %w", err)
		}
		defer func() {
			if err := os.RemoveAll(tempDir); err != nil {
				fmt.Println("Warning: failed to remove temp dir", tempDir, ":", err)
			}
		}()

		fmt.Println("Extracting", publishConfig.ociTarballPath, "to", tempDir, "...")
		if err := untar(publishConfig.ociTarballPath, tempDir); err != nil {
			return fmt.Errorf("failed to extract OCI tarball: %w", err)
		}

		store, err := oci.New(tempDir)
		if err != nil {
			return fmt.Errorf("failed to open OCI layout at %s: %w", tempDir, err)
		}

		rootDesc, err := findRootManifestDescriptor(tempDir)
		if err != nil {
			return fmt.Errorf("failed to find root manifest in OCI layout: %w", err)
		}
		fmt.Println("Found manifest to push:", rootDesc.Digest)

		fmt.Println("Publishing to", publishConfig.remoteRef, "...")
		_, err = oras.Copy(ctx, store, rootDesc.Digest.String(), repo, publishConfig.remoteRef, oras.DefaultCopyOptions)
		if err != nil {
			return fmt.Errorf("failed to publish OCI image: %w", err)
		}

		fmt.Println("Successfully published OCI image to", publishConfig.remoteRef)
		return nil
	},
}

func untar(tarballPath, destDir string) error {
	file, err := os.Open(tarballPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); err != nil {
			fmt.Println("failed to close tarball")
		}
	}()

	tr := tar.NewReader(file)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(destDir, header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			outFile, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(outFile, tr); err != nil {
				cerr := outFile.Close()
				if cerr != nil {
					fmt.Println("failed to close file:", cerr)
				}
				return err
			}
			if err := outFile.Close(); err != nil {
				fmt.Println("failed to close file:", err)
			}
		default:
			return fmt.Errorf("unsupported file type in tar: %c for file %s", header.Typeflag, header.Name)
		}
	}
	return nil
}

func findRootManifestDescriptor(layoutPath string) (ocispec.Descriptor, error) {
	indexPath := filepath.Join(layoutPath, "index.json")
	indexBytes, err := os.ReadFile(indexPath)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("could not read index.json: %w", err)
	}

	var index ocispec.Index
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("could not unmarshal index.json: %w", err)
	}

	if len(index.Manifests) == 0 {
		return ocispec.Descriptor{}, fmt.Errorf("no manifests found in index.json")
	}

	return index.Manifests[0], nil
}

func AddPublishCommand(rootCmd *cobra.Command) {
	rootCmd.AddCommand(publishCmd)
}
