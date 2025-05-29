// Copyright 2025 The Kube Resource Orchestrator Authors
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

package commands

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// Helper functions
func getKubeClient() (*kubernetes.Clientset, error) {
	kubeconfig := clientcmd.RecommendedHomeFile
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(config)
}

func diagnoseRGD() error {
	clientset, err := getKubeClient()
	if err != nil {
		return err
	}
	fmt.Println("Fetching RDG status...")
	pods, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{
		LabelSelector: "app=rdg",
	})
	if err != nil {
		return err
	}
	for _, pod := range pods.Items {
		fmt.Printf("RDG Pod: %s - Status: %s\n", pod.Name, pod.Status.Phase)
	}
	return nil
}

func editRGD() error {
	fmt.Println("Opening RDG for editing...")
	cmd := exec.Command("kubectl", "edit", "rdg")
	cmd.Stdout = cmd.Stderr
	return cmd.Run()
}

func applyRGD() error {
	fmt.Println("Applying RDG configuration...")
	cmd := exec.Command("kubectl", "apply", "-f", "rdg.yaml")
	cmd.Stdout = cmd.Stderr
	return cmd.Run()
}

func diagnoseINST() error {
	clientset, err := getKubeClient()
	if err != nil {
		return err
	}
	fmt.Println("Fetching INST status...")
	pods, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{
		LabelSelector: "app=inst",
	})
	if err != nil {
		return err
	}
	for _, pod := range pods.Items {
		fmt.Printf("INST Pod: %s - Status: %s\n", pod.Name, pod.Status.Phase)
	}
	return nil
}

func editINST() error {
	fmt.Println("Opening INST for editing...")
	cmd := exec.Command("kubectl", "edit", "inst")
	cmd.Stdout = cmd.Stderr
	return cmd.Run()
}

func applyINST() error {
	fmt.Println("Applying INST configuration...")
	cmd := exec.Command("kubectl", "apply", "-f", "inst.yaml")
	cmd.Stdout = cmd.Stderr
	return cmd.Run()
}

func showKRO() error {
	clientset, err := getKubeClient()
	if err != nil {
		return err
	}
	fmt.Println("Showing KRO deployment components...")

	fmt.Println("\n--- RDG ---")
	pods, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{
		LabelSelector: "app=rdg",
	})
	if err == nil {
		for _, pod := range pods.Items {
			fmt.Printf("RDG Pod: %s - %s\n", pod.Name, pod.Status.Phase)
		}
	}

	fmt.Println("\n--- INST ---")
	pods, err = clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{
		LabelSelector: "app=inst",
	})
	if err == nil {
		for _, pod := range pods.Items {
			fmt.Printf("INST Pod: %s - %s\n", pod.Name, pod.Status.Phase)
		}
	}

	fmt.Println("\n--- Services ---")
	services, err := clientset.CoreV1().Services("").List(context.TODO(), metav1.ListOptions{})
	if err == nil {
		for _, svc := range services.Items {
			fmt.Printf("Service: %s - %s\n", svc.Name, svc.Spec.ClusterIP)
		}
	}
	return nil
}

// Cobra command definitions
var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate the ResourceGraphDefinition",
	Long: `Validate the ResourceGraphDefinition. This command checks 
if the ResourceGraphDefinition is valid and can be used to create a ResourceGraph.`,
}

var validateRGDCmd = &cobra.Command{
	Use:   "rgd [FILE]",
	Short: "Validate a ResourceGraphDefinition file",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// TODO(DhairyaMajmudar): Implement the logic to validate the ResourceGraphDefinition file
		fmt.Println("Validation not implemented yet")
		return nil
	},
}

var diagnoseRGDCmd = &cobra.Command{
	Use:   "diagnose-rgd",
	Short: "Diagnose RDG",
	RunE: func(cmd *cobra.Command, args []string) error {
		return diagnoseRGD()
	},
}

var editRGDCmd = &cobra.Command{
	Use:   "edit-rgd",
	Short: "Edit RDG configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		return editRGD()
	},
}

var applyRGDCmd = &cobra.Command{
	Use:   "apply-rgd",
	Short: "Apply RDG configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		return applyRGD()
	},
}

var diagnoseINSTCmd = &cobra.Command{
	Use:   "diagnose-inst",
	Short: "Diagnose INST",
	RunE: func(cmd *cobra.Command, args []string) error {
		return diagnoseINST()
	},
}

var editINSTCmd = &cobra.Command{
	Use:   "edit-inst",
	Short: "Edit INST configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		return editINST()
	},
}

var applyINSTCmd = &cobra.Command{
	Use:   "apply-inst",
	Short: "Apply INST configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		return applyINST()
	},
}

var showKROCmd = &cobra.Command{
	Use:   "show",
	Short: "Show KRO deployments",
	RunE: func(cmd *cobra.Command, args []string) error {
		return showKRO()
	},
}

// AddValidateCommands registers all commands
func AddValidateCommands(rootCmd *cobra.Command) {
	validateCmd.AddCommand(validateRGDCmd)
	rootCmd.AddCommand(validateCmd)

	rootCmd.AddCommand(diagnoseRGDCmd)
	rootCmd.AddCommand(editRGDCmd)
	rootCmd.AddCommand(applyRGDCmd)
	rootCmd.AddCommand(diagnoseINSTCmd)
	rootCmd.AddCommand(editINSTCmd)
	rootCmd.AddCommand(applyINSTCmd)
	rootCmd.AddCommand(showKROCmd)
}
