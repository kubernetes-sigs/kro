package commands

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/kro-run/kro/api/v1alpha1"
	kroclient "github.com/kro-run/kro/pkg/client"
	"github.com/kro-run/kro/pkg/graph"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

var (
	resourceGroupDefinitionFile string
	outputFile                  string
	outputFormat                string
)

var generateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate the CRD from a given ResourceGroupDefinition",
	Long: "Generate the CRD from a given ResourceGroupDefinition." +
		"This command generates a CustomResourceDefinition (CRD) based on the provided ResourceGroupDefinition file.",
}

func init() {
	generateCRDCmd.PersistentFlags().StringVarP(&resourceGroupDefinitionFile, "file", "f", "",
		"Path to the ResourceGroupDefinition file")
	generateCRDCmd.PersistentFlags().StringVarP(&outputFile, "output", "o", "", "Path to the output file")
	generateCRDCmd.PersistentFlags().StringVarP(&outputFormat, "format", "F", "yaml", "Output format (yaml|json)")
}

var generateCRDCmd = &cobra.Command{
	Use:   "crd",
	Short: "Generate a CustomResourceDefinition (CRD)",
	Long: "Generate a CustomResourceDefinition (CRD) from a " +
		"ResourceGroupDefinition file. This command reads the " +
		"ResourceGroupDefinition and outputs the corresponding CRD " +
		"in the specified format.",
	RunE: func(cmd *cobra.Command, args []string) error {
		if resourceGroupDefinitionFile == "" {
			return fmt.Errorf("ResourceGroupDefinition file is required")
		}

		data, err := os.ReadFile(resourceGroupDefinitionFile)
		if err != nil {
			return fmt.Errorf("failed to read ResourceGroupDefinition file: %w", err)
		}

		var rgd v1alpha1.ResourceGraphDefinition
		if err = yaml.Unmarshal(data, &rgd); err != nil {
			return fmt.Errorf("failed to unmarshal ResourceGroupDefinition: %w", err)
		}

		if err = generateCRD(&rgd); err != nil {
			return fmt.Errorf("failed to generate CRD: %w", err)
		}

		return nil
	},
}

func generateCRD(rgd *v1alpha1.ResourceGraphDefinition) error {
	set, err := kroclient.NewSet(kroclient.Config{})
	if err != nil {
		return fmt.Errorf("failed to create client set: %w", err)
	}

	restConfig := set.RESTConfig()

	builder, err := graph.NewBuilder(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create graph builder: %w", err)
	}

	rgdGraph, err := builder.NewResourceGraphDefinition(rgd)
	if err != nil {
		return fmt.Errorf("failed to create resource graph definition: %w", err)
	}

	crd := rgdGraph.Instance.GetCRD()
	crd.SetAnnotations(map[string]string{"kro.run/version": "dev"})

	b, err := marshalObject(crd, outputFormat)
	if err != nil {
		return fmt.Errorf("failed to marshal CRD: %w", err)
	}

	if outputFile != "" {
		if err := writeFile(outputFile, b); err != nil {
			return fmt.Errorf("failed to write output file: %w", err)
		}
	} else {
		fmt.Println(string(b))
	}

	return nil
}

func writeFile(outputFile string, b []byte) error {
	if err := os.WriteFile(outputFile, b, 0644); err != nil {
		return err
	}
	fmt.Println("CRD written to", outputFile)

	return nil
}

func marshalObject(obj interface{}, outputFormat string) ([]byte, error) {
	var b []byte
	var err error

	if obj == nil {
		return nil, fmt.Errorf("nil object provided for marshaling")
	}

	switch outputFormat {
	case "json":
		b, err = json.Marshal(obj)
		if err != nil {
			return nil, err
		}
	case "yaml":
		b, err = yaml.Marshal(obj)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported output format: %s", outputFormat)
	}

	return b, nil
}

func AddGenerateCommands(rootCmd *cobra.Command) {
	generateCmd.AddCommand(generateCRDCmd)
	rootCmd.AddCommand(generateCmd)
}
