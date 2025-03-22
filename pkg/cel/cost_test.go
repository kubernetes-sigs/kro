// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package cel

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOnlyCELBudgetValues sets up values for testing
// These need to be much higher than normal to prevent test failures
var (
	TestOnlyRuntimeCELCostBudget = int64(100000000000) // 10B - very high for tests
	TestOnlyPerCallLimit         = int64(10000000)     // 10M - high enough for any test expression
)

// WithTestBudget returns program options suitable for testing,
// with much higher limits than production
func WithTestBudget() []cel.ProgramOption {
	return []cel.ProgramOption{
		cel.CostLimit(uint64(TestOnlyPerCallLimit)),
		cel.EvalOptions(cel.OptTrackCost),
	}
}

func TestWithCostTracking(t *testing.T) {
	//  WithCostTracking returns the expected number of options
	opts := WithCostTracking(10000)
	assert.Equal(t, 2, len(opts), "Expected 2 options to be returned")
}

func TestCELBudgetExceeded(t *testing.T) {
	env, err := cel.NewEnv()
	require.NoError(t, err)

	ast, iss := env.Compile(`"test"`)
	require.NoError(t, iss.Err())

	// Testing with unlimited budget (should succeed)
	opts := WithCostTracking(10000)
	program, err := env.Program(ast, opts...)
	require.NoError(t, err)

	// Evaluating with sufficient budget
	_, details, err := program.Eval(map[string]interface{}{})
	require.NoError(t, err)
	assert.NotNil(t, details)
	assert.NotNil(t, details.ActualCost())

	// Testing with minimal budget (still should pass for this trivial expression)
	opts = WithCostTracking(1)
	program, err = env.Program(ast, opts...)
	require.NoError(t, err)

	// Evaluating with minimal budget
	_, details, err = program.Eval(map[string]interface{}{})
	require.NoError(t, err)
	assert.NotNil(t, details)
	assert.NotNil(t, details.ActualCost())

	// Testing with a more complex expression that will certainly exceed a 0 budget
	ast, iss = env.Compile(`[1, 2, 3, 4, 5, 6, 7, 8, 9, 10].map(x, [1, 2, 3, 4, 5, 6, 7, 8, 9, 10].map(y, x * y)).filter(arr, arr.exists(e, e > 50))`)
	require.NoError(t, iss.Err())

	// Setting up a large budget that should allow the expression to complete
	opts = WithCostTracking(1000000)
	program, err = env.Program(ast, opts...)
	require.NoError(t, err)

	// Evaluating with large budget - should succeed
	val, details, err := program.Eval(map[string]interface{}{})
	if err == nil {
		t.Logf("Success! Expression evaluated with result: %v, cost: %v", val, *details.ActualCost())
	} else {
		t.Logf("Actual error: %v", err)
		require.Error(t, err)
		errMsg := err.Error()
		assert.Contains(t, errMsg, "cost limit exceeded", "Error should indicate cost limit exceeded")
	}

	// A tiny budget to ensure the test passes
	opts = WithCostTracking(0)
	program, err = env.Program(ast, opts...)
	require.NoError(t, err)

	// Evaluating with zero budget - should fail with budget exceeded
	_, _, err = program.Eval(map[string]interface{}{})
	require.Error(t, err)
	t.Logf("Actual error: %v", err)

	errMsg := err.Error()
	assert.Contains(t, errMsg, "cost limit exceeded", "Error should indicate cost limit exceeded")
}

func TestIsCostLimitExceeded(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "non-cost error",
			err:      errors.New("some other error"),
			expected: false,
		},
		{
			name:     "cost limit error",
			err:      errors.New("operation cancelled: actual cost limit exceeded"),
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsCostLimitExceeded(tt.err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestWrapCostLimitExceeded(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		totalCost int64
		want      string
		wantErr   bool
	}{
		{
			name:      "nil error",
			err:       nil,
			totalCost: 0,
			want:      "",
			wantErr:   false,
		},
		{
			name:      "non-cost error",
			err:       errors.New("some other error"),
			totalCost: 5000,
			want:      "some other error",
			wantErr:   true,
		},
		{
			name:      "cost limit error",
			err:       errors.New("operation cancelled: actual cost limit exceeded"),
			totalCost: 15000,
			want:      fmt.Sprintf("CEL Cost budget exceeded: total CEL cost 15000 exceeded budget of %d", RuntimeCELCostBudget),
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := WrapCostLimitExceeded(tt.err, tt.totalCost)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, tt.want, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
