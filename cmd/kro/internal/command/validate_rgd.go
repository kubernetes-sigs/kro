package command

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/cmd/kro/internal/loader"
	"github.com/kubernetes-sigs/kro/cmd/kro/internal/view"
	"github.com/kubernetes-sigs/kro/pkg/client"
	"github.com/kubernetes-sigs/kro/pkg/graph"
)

type ValidateRGDOptions struct {
	Path string
}

func NewValidateRGDCommand(cli *CLI) *cobra.Command {
	var opts ValidateRGDOptions

	cmd := &cobra.Command{
		Use:   "rgd",
		Short: "Validate a ResourceGraphDefinition",
		Long: Highlight("kro validate rgd -f <path>") + "\n\n" +
			"Validate a ResourceGraphDefinition by file or directory.\n\n" +
			"Performs both syntactic and semantic validation of RGD resources.\n" +
			"When targeting a directory, all .yaml and .yml files will be validated.\n\n" +
			"Examples:\n" +
			"  # Validate a single RGD file\n" +
			"  kro validate rgd -f example.yaml\n\n" +
			"  # Validate all RGDs in a directory\n" +
			"  kro validate rgd -f .\n",
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunValidateRGD(cmd.Context(), cli, opts)
		},
	}

	cmd.Flags().StringVarP(&opts.Path, "file", "f", "", "Path to RGD file or directory")
	cmd.MarkFlagRequired("file")

	return cmd
}

func RunValidateRGD(ctx context.Context, cli *CLI, opts ValidateRGDOptions) error {
	validateView := view.NewValidateView(cli.Viewer)

	results, err := loader.LoadResourceGraphDefinitionsDetailed(opts.Path)
	if err != nil {
		return err
	}

	if len(results) == 0 {
		return fmt.Errorf("no YAML files found in %q", opts.Path)
	}

	// Create client set for validation
	set, err := client.NewSet(client.Config{})
	if err != nil {
		return fmt.Errorf("failed to create client set: %w", err)
	}

	// Create graph builder for validation
	builder, err := graph.NewBuilder(set.RESTConfig(), set.HTTPClient())
	if err != nil {
		return fmt.Errorf("failed to create graph builder: %w", err)
	}

	resultView := view.ValidateResult{FileCount: len(results)}

	for _, result := range results {
		if result.Err != nil {
			resultView.Errors = append(resultView.Errors, view.ValidateFileError{File: result.Path, Message: result.Err.Error()})
			continue
		}

		if err := validateRGD(result.RGD, builder); err != nil {
			resultView.Errors = append(resultView.Errors, view.ValidateFileError{File: result.Path, Message: err.Error()})
		}
	}

	validateView.Render(resultView)
	if resultView.HasErrors() {
		return errors.New("")
	}
	return nil
}

func validateRGD(rgd *v1alpha1.ResourceGraphDefinition, builder *graph.Builder) error {
	if _, err := builder.NewResourceGraphDefinition(rgd); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	return nil
}
