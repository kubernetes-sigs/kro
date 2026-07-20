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
	"os"
	"path/filepath"

	"github.com/google/yamlfmt/formatters/basic"
	"github.com/spf13/cobra"
)

var (
	checkOnly bool
	filePath  string
)

var fmtCmd = &cobra.Command{
	Use:   "fmt",
	Short: "Format YAML files in an opinionated way",
	Long:  `Format YAML files with kro's opinionated style (2-space indentation, gofmt-style blank lines).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if filePath == "" {
			return fmt.Errorf("file path is required")
		}

		changed, err := formatFile(filePath, checkOnly)
		if err != nil {
			return fmt.Errorf("failed to format %s: %w", filePath, err)
		}

		if checkOnly && changed {
			fmt.Fprintf(os.Stderr, "%s needs formatting\n", filePath)
			fmt.Fprintf(os.Stderr, "\nRun 'kro fmt -f %s' to fix\n", filePath)
			os.Exit(1)
		}

		return nil
	},
}

func init() {
	fmtCmd.Flags().StringVarP(&filePath, "file", "f", "", "Path to the YAML file")
	fmtCmd.Flags().BoolVar(&checkOnly, "check", false,
		"Check if file needs formatting (exit 1 if changes needed)")
}

func formatFile(path string, checkOnly bool) (changed bool, err error) {
	sourceYAML, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}

	factory := &basic.BasicFormatterFactory{}
	config := map[string]interface{}{
		"indent":                     2,
		"retain_line_breaks_single":  true,
		"trim_trailing_whitespace":   true,
		"eof_newline":                true,
		"pad_line_comments":          1,
	}

	formatter, err := factory.NewFormatter(config)
	if err != nil {
		return false, fmt.Errorf("failed to create formatter: %w", err)
	}

	formatted, err := formatter.Format(sourceYAML)
	if err != nil {
		return false, fmt.Errorf("failed to format: %w", err)
	}

	// Check if formatting changed anything
	if bytes.Equal(sourceYAML, formatted) {
		return false, nil
	}

	if checkOnly {
		return true, nil
	}

	// Atomic write: temp file + rename
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kro-fmt.tmp.")
	if err != nil {
		return true, err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(formatted); err != nil {
		tmp.Close()
		return true, err
	}
	if err := tmp.Close(); err != nil {
		return true, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return true, err
	}
	fmt.Printf("formatted %s\n", path)

	return true, nil
}

func AddFmtCommands(rootCmd *cobra.Command) {
	rootCmd.AddCommand(fmtCmd)
}
