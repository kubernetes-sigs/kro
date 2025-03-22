package cel

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
)

const (
	// PerCallLimit specifies the cost limit per individual CEL expression evaluation
	// This gives roughly 0.1 second of execution time per expression evaluation
	PerCallLimit = 1000000

	// RuntimeCELCostBudget is the total cost budget allowed during a single
	// ResourceGroup reconciliation cycle. This includes all expression evaluations
	// across all resources defined in the ResourceGroup. The budget gives roughly
	// 1 second of total execution time before being exceeded.
	RuntimeCELCostBudget = 1000

	// CheckFrequency configures the number of iterations within a comprehension to evaluate
	// before checking whether the function evaluation has been interrupted
	CheckFrequency = 100
)

var ErrCELBudgetExceeded = errors.New("CEL Cost budget exceeded")

// IsCostLimitExceeded checks if the error is related to CEL cost limit exceeding
func IsCostLimitExceeded(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "cost limit exceeded")
}

// WrapCostLimitExceeded wraps a CEL cost limit error with our standard ErrCELBudgetExceeded
// If the error is not a cost limit error, it returns the original error
func WrapCostLimitExceeded(err error, totalCost int64) error {
	if err == nil {
		return nil
	}

	if IsCostLimitExceeded(err) {
		return errors.New(ErrCELBudgetExceeded.Error() + ": total CEL cost " +
			fmt.Sprintf("%d", totalCost) + " exceeded budget of " +
			fmt.Sprintf("%d", RuntimeCELCostBudget))
	}

	return err
}

func WithCostTracking(costLimit int64) []cel.ProgramOption {
	return []cel.ProgramOption{
		cel.CostLimit(uint64(costLimit)),
		cel.EvalOptions(cel.OptTrackCost),
	}
}
