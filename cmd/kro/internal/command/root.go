package command

import (
	"os"
	"strings"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/kubernetes-sigs/kro/cmd/kro/internal/view"
	"github.com/kubernetes-sigs/kro/cmd/kro/version"
)

var (
	jsonFlag  bool
	debugFlag bool
	rootCmd   *cobra.Command
)

func NewRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "kro",
		Short: color.RGB(50, 108, 229).Sprintf("kro [global options] <subcommand> [args]") + `\n` +
			"A CLI utility providing workflows for working with kro ResourceGraphDefinitions",
		Long: color.RGB(50, 108, 229).Sprintf("Usage: kro [global options] <subcommand> [args]\n") +
			`
 __
|  | _________  ____
|  |/ /\_  __ \/  _ \
|    <  |  | \(  <_> )
|__|_ \ |__|   \____/
     \/
		` + "\n" +
			"kro is a CLI utility that provides various workflows for working with\n" +
			"ResourceGraphDefinitions (RGDs) as well as their instances. It includes\n" +
			"commands for generation, validation, distribution, and hydration of these resources.\n\n",
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Run: func(cmd *cobra.Command, args []string) {
			if len(args) == 0 {
				_ = cmd.Help()
			}
		},
	}

	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.PersistentFlags().BoolVar(&jsonFlag, "json", false, "Output in JSON format")
	cmd.PersistentFlags().BoolVar(&debugFlag, "debug", false, "Set log level to debug")
	return cmd
}

func setCobraUsageTemplate() {
	cobra.AddTemplateFunc("StyleHeading", color.RGB(50, 108, 229).SprintFunc())
	usageTemplate := rootCmd.UsageTemplate()
	usageTemplate = strings.NewReplacer(
		`Usage:`, `{{StyleHeading "Usage:"}}`,
		`Examples:`, `{{StyleHeading "Examples:"}}`,
		`Available Commands:`, `{{StyleHeading "Available Commands:"}}`,
		`Additional Commands:`, `{{StyleHeading "Additional Commands:"}}`,
		`Flags:`, `{{StyleHeading "Options:"}}`,
		`Global Flags:`, `{{StyleHeading "Global Options:"}}`,
	).Replace(usageTemplate)
	rootCmd.SetUsageTemplate(usageTemplate)
}

func setVersionTemplate() {
	rootCmd.SetVersionTemplate("{{.Version}}")
}

func Execute() {
	rootCmd = NewRootCommand()

	// Templates are used to standardize the output format of kro.
	setCobraUsageTemplate()
	setVersionTemplate()

	// Disable color output if NO_COLOR is set in the environment
	if _, exists := os.LookupEnv("NO_COLOR"); exists {
		color.NoColor = true
	} else {
		color.NoColor = false
	}

	// Create a temporary CLI instance with default settings
	// The viewer will be reconfigured in PersistentPreRun after flags are parsed
	cli := NewCLI(view.ViewHuman, os.Stdout, view.LogLevelSilent)

	// Add all subcommands to the root command
	AddCommands(rootCmd, cli)

	// Configure viewer after flags are parsed by Cobra
	rootCmd.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		// Set up the view type based on the `--json` flag
		viewType := view.ViewHuman
		if jsonFlag {
			viewType = view.ViewJSON
		}

		logLevel := view.LogLevelSilent
		logEnv := os.Getenv("KRO_LOG")
		switch strings.ToLower(logEnv) {
		case "debug":
			logLevel = view.LogLevelDebug
		case "info":
			logLevel = view.LogLevelInfo
		default:
			// Unknown value: keep default (silent)
		}
		if debugFlag {
			logLevel = view.LogLevelDebug
		}

		// Update the CLI viewer with the correct configuration
		s := view.NewStream(os.Stdout)
		cli.Viewer = view.NewViewer(viewType, s, logLevel)
		cli.Stream = s
	}

	// Walk and execute the resolved command with flags.
	if err := rootCmd.Execute(); err != nil {
		if msg := err.Error(); msg != "" {
			cli.Println(msg)
		}
		os.Exit(1)
	}

	os.Exit(0)
}

// AddCommands registers all subcommands to the root command.
func AddCommands(root *cobra.Command, cli *CLI) {
	root.AddCommand(
		NewVersionCommand(cli),
		NewValidateCommand(cli),
	)
}
