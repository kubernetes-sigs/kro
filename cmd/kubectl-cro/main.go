package main

import (
	"os"

	"github.com/kro-run/kro/cmd/kubectl-cro/render"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "kubectl-cro",
	Short: "kubectl-cro is a kubectl plugin for kro",
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(render.Cmd)
}
