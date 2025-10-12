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
	"flag"
	"fmt"
	"io/ioutil"
	"log"

	"github.com/kubernetes-sigs/kro/simpleschema"
	"github.com/kubernetes-sigs/kro/simpleschema/crd"
	"sigs.k8s.io/yaml" // Import the k8s-specific YAML package
)

// InputFile represents the structure of the input schema file,
// mirroring api/v1alpha1.Schema.
type InputFile struct {
	Kind       string                 `yaml:"kind"`
	APIVersion string                 `yaml:"apiVersion"`
	Group      string                 `yaml:"group"`
	Spec       map[string]interface{} `yaml:"spec"`
	Status     map[string]interface{} `yaml:"status"`
	Types      map[string]interface{} `yaml:"types"`
	Scope      string                 `yaml:"scope"`
}

func main() {
	var inputFile, outputFile string
	flag.StringVar(&inputFile, "input", "", "Path to the simple schema file.")
	flag.StringVar(&outputFile, "file", "", "Path to the output file. If not provided, output to stdout.")
	flag.Parse()

	if inputFile == "" {
		log.Fatal("-input flag is required")
	}

	data, err := ioutil.ReadFile(inputFile)
	if err != nil {
		log.Fatalf("failed to read input file: %v", err)
	}

	var input InputFile
	if err := yaml.Unmarshal(data, &input); err != nil {
		log.Fatalf("failed to unmarshal YAML: %v", err)
	}

	if input.Kind == "" {
		log.Fatal("kind is required in the input file")
	}
	if input.APIVersion == "" {
		log.Fatal("apiVersion is required in the input file")
	}
	if input.Group == "" {
		input.Group = "kro.run"
	}

	specSchema, err := simpleschema.ToOpenAPISpec(input.Spec, input.Types)
	if err != nil {
		log.Fatalf("failed to convert spec to OpenAPI spec: %v", err)
	}

	statusSchema, err := simpleschema.ToOpenAPISpec(input.Status, input.Types)
	if err != nil {
		log.Fatalf("failed to convert status to OpenAPI spec: %v", err)
	}

	crd := crd.SynthesizeCRD(input.Group, input.APIVersion, input.Kind, *specSchema, *statusSchema, true, nil)

	// Marshal the Deployment object to YAML
	yamlBytes, err := yaml.Marshal(crd)
	if err != nil {
		fmt.Printf("Error marshalling to YAML: %v\n", err)
		return
	}

	if outputFile == "" {
		fmt.Println(string(yamlBytes))
	} else {
		err := ioutil.WriteFile(outputFile, yamlBytes, 0644)
		if err != nil {
			log.Fatalf("failed to write output file: %v", err)
		}
	}
}
