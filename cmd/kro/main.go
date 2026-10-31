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

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kubernetes-sigs/kro/cmd/kro/commands"
	"github.com/kubernetes-sigs/kro/cmd/kro/internal/kubeconfig"
)

func main() {
	rootCmd := NewRootCommand()
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type CommandOptions struct {
	Verbose bool
}

var cmd = &cobra.Command{
	Use:   "kro",
	Short: "kro- Kube Resource Orchestrator CLI",
	Long: `kro CLI helps developers and administrators manage 
ResourceGraphDefinitions (RGDs) and their instances in Kubernetes cluster.`,
	SilenceErrors: true,
	SilenceUsage:  true,
}

var opts = &CommandOptions{}

func NewRootCommand() *cobra.Command {
	// Global flags. The cluster connection flags are bound to the shared
	// kubeconfig options so that subcommands can build their clients from them.
	cmd.PersistentFlags().StringVar(&kubeconfig.Global.KubeConfigPath, "kubeconfig", "",
		"Path to kubeconfig file (defaults to $KUBECONFIG, then ~/.kube/config)")
	cmd.PersistentFlags().StringVar(&kubeconfig.Global.Context, "context", "",
		"Kubernetes context to use (defaults to the kubeconfig's current-context)")
	cmd.PersistentFlags().BoolVar(&opts.Verbose, "verbose", false, "Enable verbose logging")

	// Cli commands
	commands.AddCommands(cmd)
	return cmd
}
