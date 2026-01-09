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

// IsDataPending returns true if the error indicates data is pending and
// evaluation should be retried later.
func IsDataPending(err error) bool {
	return errors.Is(err, ErrDataPending)
}

// celDataPendingPatterns are CEL error patterns that indicate data is not yet
// available (retryable). Other CEL errors are considered expression bugs.
//
// Data pending (retryable):
//   - "no such key"       : map key doesn't exist (e.g., status.field not populated)
//   - "no such field"     : struct field doesn't exist yet
//   - "index out of range": list doesn't have enough items yet
//     (could also be a bug, but we assume good intention - the list will grow)
//
// NOT data pending (expression bugs):
//   - "type conversion error" : wrong types in expression
//   - "no such overload"      : invalid operation for types
//   - "division by zero"      : math error
var celDataPendingPatterns = []string{
	"no such key",
	"no such field",
	"index out of range",
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
