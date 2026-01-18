package command

import (
	"github.com/spf13/cobra"
)

// ValidateOptions holds the options for the deploy command.
type ValidateOptions struct {
	Path     string
	VarFiles []string
}

func NewValidateCommand(cli *CLI) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate [subcommand]",
		Short: "Validate ResourceGraphDefinitions and instances",
		Long: Highlight("kro validate [subcommand]") + "\n\n" +
			"Validate ResourceGraphDefinitions (RGDs) and their instances.\n\n" +
			"Performs both syntactic and semantic validation of resources.\n" +
			"You can validate a single file or an entire directory of YAML files.\n",
	}

	// Add subcommands
	cmd.AddCommand(
		NewValidateRGDCommand(cli),
	)

	return cmd
}
