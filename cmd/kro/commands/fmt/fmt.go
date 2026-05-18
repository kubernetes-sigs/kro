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

package fmt

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	yaml "go.yaml.in/yaml/v3"
)

// TODO: Consider vendoring/forking sigs.k8s.io/yaml/yamlfmt if we need
// custom kro-specific formatting logic (e.g., enforce field ordering,
// ResourceGraphDefinition-specific validation during formatting, etc.)

var fmtCmd = &cobra.Command{
	Use:   "fmt",
	Short: "Format YAML files",
	Long: `Format YAML files with consistent style. ` +
		`Uses 2-space indentation and block-style formatting.`,
}

var (
	files      []string
	checkOnly  bool
	inPlace    bool
	recursive  bool
)

func init() {
	fmtRGDCmd.PersistentFlags().StringSliceVarP(&files, "file", "f", []string{},
		"Path to YAML file(s) to format")
	fmtRGDCmd.PersistentFlags().BoolVar(&checkOnly, "check", false,
		"Check if files need formatting (exit 1 if changes needed)")
	fmtRGDCmd.PersistentFlags().BoolVarP(&inPlace, "write", "w", false,
		"Write formatted output to input files (default: stdout)")
	fmtRGDCmd.PersistentFlags().BoolVarP(&recursive, "recursive", "r", false,
		"Recursively format all YAML files in directories")
}

var fmtRGDCmd = &cobra.Command{
	Use:   "rgd",
	Short: "Format ResourceGraphDefinition YAML files",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(files) == 0 && len(args) == 0 {
			return fmt.Errorf("no files specified (use -f or provide file arguments)")
		}

		// Combine flags and args
		allFiles := append(files, args...)

		// Expand directories if recursive
		var expandedFiles []string
		for _, path := range allFiles {
			info, err := os.Stat(path)
			if err != nil {
				return fmt.Errorf("failed to stat %s: %w", path, err)
			}

			if info.IsDir() {
				if !recursive {
					return fmt.Errorf("%s is a directory (use -r for recursive)", path)
				}
				err := filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
					if err != nil {
						return err
					}
					if !info.IsDir() && (filepath.Ext(p) == ".yaml" || filepath.Ext(p) == ".yml") {
						expandedFiles = append(expandedFiles, p)
					}
					return nil
				})
				if err != nil {
					return fmt.Errorf("failed to walk directory %s: %w", path, err)
				}
			} else {
				expandedFiles = append(expandedFiles, path)
			}
		}

		needsFormatting := false
		hadErrors := false
		for _, path := range expandedFiles {
			changed, err := formatFile(path, checkOnly, inPlace)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to format %s: %v\n", path, err)
				hadErrors = true
				continue
			}
			if changed {
				needsFormatting = true
				if checkOnly {
					fmt.Fprintf(os.Stderr, "%s needs formatting\n", path)
				}
			}
		}

		if hadErrors {
			fmt.Fprintln(os.Stderr, "\nSome files could not be formatted (see errors above)")
		}

		if checkOnly && needsFormatting {
			fmt.Fprintln(os.Stderr, "\nRun 'kro fmt rgd -f <files> -w' to fix")
			os.Exit(1)
		}

		return nil
	},
}

func formatFile(path string, checkOnly, inPlace bool) (changed bool, err error) {
	sourceYAML, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}

	formatted := &bytes.Buffer{}
	if err := formatYAML(bytes.NewReader(sourceYAML), formatted); err != nil {
		return false, err
	}

	// Check if formatting changed anything
	if bytes.Equal(sourceYAML, formatted.Bytes()) {
		return false, nil
	}

	if checkOnly {
		return true, nil
	}

	if inPlace {
		// Atomic write: temp file + rename
		tmp, err := os.CreateTemp(filepath.Dir(path), ".kro-fmt.tmp.")
		if err != nil {
			return true, err
		}
		defer os.Remove(tmp.Name())

		if _, err := tmp.Write(formatted.Bytes()); err != nil {
			tmp.Close()
			return true, err
		}
		if err := tmp.Close(); err != nil {
			return true, err
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			return true, err
		}
		if os.Getenv("KROFMT_QUIET") == "" {
			fmt.Printf("formatted %s\n", path)
		}
	} else {
		fmt.Print(formatted.String())
	}

	return true, nil
}

func formatYAML(in io.Reader, out io.Writer) error {
	decoder := yaml.NewDecoder(in)
	encoder := yaml.NewEncoder(out)
	encoder.SetIndent(2)

	for {
		var node yaml.Node
		if err := decoder.Decode(&node); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("failed to decode YAML: %w", err)
		}
		setBlockStyle(&node)
		if err := encoder.Encode(&node); err != nil {
			return fmt.Errorf("failed to encode YAML: %w", err)
		}
	}

	return nil
}

func setBlockStyle(node *yaml.Node) {
	node.Style = node.Style & (^yaml.FlowStyle)
	for _, child := range node.Content {
		setBlockStyle(child)
	}
}

func AddFmtCommands(rootCmd *cobra.Command) {
	fmtCmd.AddCommand(fmtRGDCmd)
	rootCmd.AddCommand(fmtCmd)
}
