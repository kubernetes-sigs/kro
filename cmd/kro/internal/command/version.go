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

package command

import (
	"github.com/spf13/cobra"
)

// VersionOptions holds the options for the version command.
type VersionOptions struct {
	Path string
}

func NewVersionCommand(cli *CLI) *cobra.Command {
	opts := VersionOptions{
		Path: ".", // Default to current directory
	}

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show version information",
		Long: Highlight("kro version") + "\n\n" +
			"Display the current version of kro.\n\n" +
			"This information is useful for bug reports, ensuring team\n" +
			"consistency, and verifying compatibility with documentation\n" +
			"and automation scripts.\n",
		Args: MaxArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			if len(args) > 0 {
				opts.Path = args[0]
			}

			cli.PrintVersion()
		},
	}
	return cmd
}
