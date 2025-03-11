package render

import (
	"github.com/kro-run/kro/cmd/kubectl-cro/render/crd"
	"github.com/spf13/cobra"
)

var Cmd = &cobra.Command{
	Use:   "render",
	Short: "Client-side rendering of kro resources for use in validation and testing",
}

func init() {
	Cmd.AddCommand(crd.Cmd)
}
