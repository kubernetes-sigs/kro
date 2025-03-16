package main

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"

	rgdv1alpha1 "github.com/aws/resource-graph-definition-controller/api/v1alpha1"
	rgdclient "github.com/aws/resource-graph-definition-controller/pkg/client"
)

type ResourceNode struct {
	ID           string
	Status       string
	Dependencies []string
	Children     []*ResourceNode
}

func buildResourceTree(resources []rgdv1alpha1.ResourceInformation) []*ResourceNode {
	// Create a map of id to ResourceNode
	nodeMap := make(map[string]*ResourceNode)
	
	// First pass: Create nodes
	for _, res := range resources {
		node := &ResourceNode{
			ID:           res.ID,
			Status:       res.Status,
			Dependencies: make([]string, 0),
			Children:     make([]*ResourceNode, 0),
		}
		if len(res.Dependencies) > 0 {
			for _, dep := range res.Dependencies {
				node.Dependencies = append(node.Dependencies, dep.ID)
			}
		}
		nodeMap[res.ID] = node
	}

	// Second pass: Build tree structure
	var roots []*ResourceNode
	for _, node := range nodeMap {
		if len(node.Dependencies) == 0 {
			roots = append(roots, node)
		} else {
			for _, depID := range node.Dependencies {
				if parent, exists := nodeMap[depID]; exists {
					parent.Children = append(parent.Children, node)
				}
			}
		}
	}

	return roots
}

func printTree(node *ResourceNode, prefix string) {
	fmt.Printf("%s%s [Status: %s]\n", prefix, node.ID, node.Status)
	for _, child := range node.Children {
		printTree(child, prefix+"  ")
	}
}

func getResourceGraphTree(namespace, name string) error {
	config, err := clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
	if err != nil {
		return fmt.Errorf("error building kubeconfig: %v", err)
	}

	rgdClient, err := rgdclient.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("error creating RGD client: %v", err)
	}

	rgd, err := rgdClient.ResourcegraphdefinitionV1alpha1().ResourceGraphDefinitions(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting RGD: %v", err)
	}

	roots := buildResourceTree(rgd.Status.Resources)
	
	fmt.Printf("Resource Graph Definition Tree for %s/%s:\n", namespace, name)
	for _, root := range roots {
		printTree(root, "")
	}

	return nil
}