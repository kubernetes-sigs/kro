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

package runtime

import (
	"errors"
	"strings"
)

// ErrDataPending indicates that CEL evaluation failed because required data
// is not yet available (e.g., a resource's status field hasn't been populated).
// This is a retryable condition - the controller should requeue and try again.
var ErrDataPending = errors.New("data pending")

// ErrDesiredNotResolved indicates that the desired state for a node has not
// been resolved yet. This typically happens when GetDesired is called on a
// node that is still in Pending or Error state.
var ErrDesiredNotResolved = errors.New("desired state not resolved")

// IsDataPending returns true if the error indicates data is pending and
// evaluation should be retried later.
func IsDataPending(err error) bool {
	return errors.Is(err, ErrDataPending)
}

// celDataPendingPatterns are CEL error patterns that indicate data is not yet
// available (retryable). Other CEL errors are considered expression bugs.
//
// Data pending (retryable):
//   - "no such key"        : map key doesn't exist (e.g., status.field not populated)
//   - "no such field"      : struct field doesn't exist yet
//   - "no such attribute"  : dependency resource not yet in context
//   - "index out of bounds": list doesn't have enough items yet
//
// NOT data pending (expression bugs):
//   - "type conversion error" : wrong types in expression
//   - "no such overload"      : invalid operation for types
//   - "division by zero"      : math error
var celDataPendingPatterns = []string{
	"no such key",
	"no such field",
	"no such attribute",
	"index out of bounds",
}

// isCELDataPending checks if a CEL error indicates data is pending.
func isCELDataPending(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, pattern := range celDataPendingPatterns {
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}
