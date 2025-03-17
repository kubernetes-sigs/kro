package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"slices"
)

// CelMetrics stores the cost of CEL expression evaluations
type CelMetrics struct {
	TotalCost       int            `json:"totalCost,omitempty"`
	CostPerResource map[string]int `json:"costPerResource,omitempty"`
}

// Modify ResourceGroupStatus to include CelMetrics
type ResourceGroupStatus struct {
	// Existing fields...

	CelMetrics *CelMetrics `json:"celMetrics,omitempty"`
}
