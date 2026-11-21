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

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/kubernetes-sigs/kro/cmd/kro/commands"
	"github.com/kubernetes-sigs/kro/cmd/kro/internal/kubeconfig"
	"github.com/kubernetes-sigs/kro/pkg/features"
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
	// Global flags
	kubeconfig.AddFlags(cmd.PersistentFlags())
	cmd.PersistentFlags().BoolVar(&opts.Verbose, "verbose", false, "Enable verbose logging")
	cmd.PersistentPreRun = func(*cobra.Command, []string) { setLogger(opts.Verbose) }
	// Expose the same --feature-gates flag as the controller so that validate
	// and generate accept RGDs that use gated features (e.g. omit()).
	features.FeatureGate.AddFlag(cmd.PersistentFlags())

	// Cli commands
	commands.AddCommands(cmd)
	return cmd
}

// setLogger sends the logs of the packages that the CLI uses, controller-runtime's
// included, to stderr with --verbose, and drops them otherwise.
func setLogger(verbose bool) {
	if !verbose {
		ctrllog.SetLogger(logr.Discard())
		return
	}
	ctrllog.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))
}
