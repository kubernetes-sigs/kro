package utils

import (
	"time"

	"github.com/your-org/kro/pkg/cel/metrics"
)

// CELCompilationTimer is a helper for timing CEL compilations
type CELCompilationTimer struct {
	resourceType   string
	expressionType string
	startTime      time.Time
}

// NewCELCompilationTimer creates a new CEL compilation timer
func NewCELCompilationTimer(resourceType, expressionType string) *CELCompilationTimer {
	return &CELCompilationTimer{
		resourceType:   resourceType,
		expressionType: expressionType,
		startTime:      time.Now(),
	}
}

// ObserveSuccess observes a successful compilation and records the elapsed time
func (t *CELCompilationTimer) ObserveSuccess() {
	elapsed := time.Since(t.startTime).Seconds()
	metrics.RecordCELCompilationLatency(t.resourceType, t.expressionType, elapsed)
	metrics.RecordCELCompilationSuccess(t.resourceType, t.expressionType)
}

// ObserveFailure observes a failed compilation and records the elapsed time
func (t *CELCompilationTimer) ObserveFailure() {
	elapsed := time.Since(t.startTime).Seconds()
	metrics.RecordCELCompilationLatency(t.resourceType, t.expressionType, elapsed)
	metrics.RecordCELCompilationFailure(t.resourceType, t.expressionType)
} 