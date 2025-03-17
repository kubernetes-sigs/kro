package crd

import (
	"context"
	"log"

	"github.com/kro-run/api/v1alpha1"
)

// UpdateResourceGroupStatus updates the status of a ResourceGroup with CEL cost metrics.
func UpdateResourceGroupStatus(rg *v1alpha1.ResourceGroup, celCosts map[string]int) {
	totalCost := 0
	for _, cost := range celCosts {
		totalCost += cost
	}

	// Set the calculated CEL cost metrics in ResourceGroup status
	rg.Status.CelMetrics = &v1alpha1.CelMetrics{
		TotalCost:       totalCost,
		CostPerResource: celCosts,
	}

	// Update the status in Kubernetes
	err := r.Status().Update(context.TODO(), rg)
	if err != nil {
		log.Printf("Failed to update ResourceGroup status: %v", err)
	}
}
