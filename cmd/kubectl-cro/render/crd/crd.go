package crd

import (
	"github.com/spf13/cobra"
)

var Cmd = &cobra.Command{
	Use:   "crd",
	Short: "Render a resource graph definition into a CRD",
}

// TODO(tomasaschan): implement the render crd command
