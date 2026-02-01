package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	kroclient "github.com/kubernetes-sigs/kro/pkg/client"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func main() {
	rgdFile := ""
	instanceFile := ""
	outputFormat := "yaml"

	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "-f", "--file":
			i++
			if i < len(os.Args) {
				rgdFile = os.Args[i]
			}
		case "-i", "--instance":
			i++
			if i < len(os.Args) {
				instanceFile = os.Args[i]
			}
		case "-o", "--output":
			i++
			if i < len(os.Args) {
				outputFormat = os.Args[i]
			}
		case "-h", "--help":
			printUsage()
			os.Exit(0)
		}
	}

	if rgdFile == "" || instanceFile == "" {
		fmt.Fprintf(os.Stderr, "Error: -f (RGD file) and -i (instance file) are required\n")
		printUsage()
		os.Exit(1)
	}

	if err := run(rgdFile, instanceFile, outputFormat); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `kro-render - Offline dry-run render of RGD template with instance data

Usage:
  kro-render -f <rgd.yaml> -i <instance.yaml> [-o yaml|json]

Options:
  -f, --file      Path to ResourceGraphDefinition file (required)
  -i, --instance  Path to instance file, or '-' for stdin (required)
  -o, --output    Output format: yaml (default) or json

Examples:
  kro-render -f empty.rgd.yaml -i empty.yaml
  kro generate instance -f empty.rgd.yaml | kro-render -f empty.rgd.yaml -i -

`)
}

func run(rgdPath, instancePath, outputFormat string) error {
	rgdData, err := os.ReadFile(rgdPath)
	if err != nil {
		return fmt.Errorf("read RGD file: %w", err)
	}

	var rgd v1alpha1.ResourceGraphDefinition
	if err := yaml.Unmarshal(rgdData, &rgd); err != nil {
		return fmt.Errorf("unmarshal RGD: %w", err)
	}

	var instanceData []byte
	if instancePath == "-" {
		scanner := bufio.NewScanner(os.Stdin)
		var b strings.Builder
		for scanner.Scan() {
			b.WriteString(scanner.Text())
			b.WriteByte('\n')
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		instanceData = []byte(b.String())
	} else {
		instanceData, err = os.ReadFile(instancePath)
		if err != nil {
			return fmt.Errorf("read instance file: %w", err)
		}
	}

	instance, err := decodeInstance(instanceData)
	if err != nil {
		return fmt.Errorf("decode instance: %w", err)
	}

	g, err := createGraph(&rgd)
	if err != nil {
		return fmt.Errorf("create graph: %w", err)
	}

	rt, err := runtime.FromGraph(g, instance)
	if err != nil {
		return fmt.Errorf("create runtime: %w", err)
	}

	var allResources []*unstructured.Unstructured
	for _, node := range rt.Nodes() {
		ignored, err := node.IsIgnored()
		if err != nil {
			return fmt.Errorf("node %q includeWhen: %w", node.Spec.Meta.ID, err)
		}
		if ignored {
			continue
		}

		desired, err := node.GetDesired()
		if err != nil {
			if runtime.IsDataPending(err) {
				return fmt.Errorf("node %q: dry-run cannot satisfy readyWhen/dependencies (data pending). Use RGDs without cross-resource readyWhen for offline render", node.Spec.Meta.ID)
			}
			return fmt.Errorf("node %q: %w", node.Spec.Meta.ID, err)
		}

		allResources = append(allResources, desired...)
	}

	return writeResources(allResources, outputFormat)
}

func decodeInstance(data []byte) (*unstructured.Unstructured, error) {
	parts := strings.SplitN(string(data), "\n---\n", 2)
	doc := strings.TrimSpace(parts[0])
	if doc == "" {
		return nil, fmt.Errorf("empty instance input")
	}

	var obj unstructured.Unstructured
	if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
		return nil, err
	}
	if len(obj.Object) == 0 {
		return nil, fmt.Errorf("empty or invalid instance document")
	}
	return &obj, nil
}

func createGraph(rgd *v1alpha1.ResourceGraphDefinition) (*graph.Graph, error) {
	set, err := kroclient.NewSet(kroclient.Config{})
	if err != nil {
		return nil, fmt.Errorf("create client set: %w", err)
	}

	builder, err := graph.NewBuilder(set.RESTConfig(), set.HTTPClient())
	if err != nil {
		return nil, fmt.Errorf("create graph builder: %w", err)
	}

	g, err := builder.NewResourceGraphDefinition(rgd)
	if err != nil {
		return nil, fmt.Errorf("build resource graph: %w", err)
	}

	return g, nil
}

func writeResources(resources []*unstructured.Unstructured, format string) error {
	for i, obj := range resources {
		var b []byte
		var err error
		switch format {
		case "json":
			b, err = json.MarshalIndent(obj.Object, "", "  ")
		case "yaml":
			b, err = yaml.Marshal(obj.Object)
		default:
			return fmt.Errorf("unsupported format: %s (use yaml or json)", format)
		}
		if err != nil {
			return err
		}
		if i > 0 && format == "yaml" {
			fmt.Println("---")
		}
		fmt.Println(string(b))
	}
	return nil
}
