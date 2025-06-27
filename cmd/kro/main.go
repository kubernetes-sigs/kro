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

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kro-run/kro/cmd/kro/commands"
	"github.com/spf13/cobra"
	"k8s.io/client-go/util/homedir"
)

func main() {
	rootCmd := NewRootCommand()
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type CommandOptions struct {
	KubeConfigPath string
	Context        string
	Verbose        bool
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

	if home := homedir.HomeDir(); home != "" {
		opts.KubeConfigPath = filepath.Join(home, ".kube", "config")
	}

	// Global flags
	cmd.PersistentFlags().StringVar(&opts.KubeConfigPath, "kubeconfig", opts.KubeConfigPath, "Path to kubeconfig file")
	cmd.PersistentFlags().StringVar(&opts.Context, "context", "", "Kubernetes context to use")
	cmd.PersistentFlags().BoolVar(&opts.Verbose, "verbose", false, "Enable verbose logging")

	// TODO: Command groups
	commands.AddValidateCommands(cmd)
	commands.AddGenerateCommands(cmd)
	return cmd
}
