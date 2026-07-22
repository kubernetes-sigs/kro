// Copyright 2026 The Kubernetes Authors.
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

// ErrDataPending indicates a CEL evaluation failed because required data
// is not yet available — e.g. a status field the cluster hasn't populated
// yet. The reconciler treats this as a soft requeue rather than a
// permanent error.
var ErrDataPending = errors.New("data pending")

// ErrWaitingForReadiness indicates a node has been applied but its
// readyWhen conditions are not satisfied yet. The reconciler requeues.
var ErrWaitingForReadiness = errors.New("waiting for readiness")

// ErrDesiredNotResolved indicates Resolve hasn't been called for a node
// the caller is trying to inspect. Internal — surfaced only when callers
// reach into runtime out of order.
var ErrDesiredNotResolved = errors.New("desired state not resolved")

// IsDataPending reports whether an error indicates a retryable
// data-not-yet-available condition.
func IsDataPending(err error) bool { return errors.Is(err, ErrDataPending) }

// celDataPendingPatterns are CEL error message fragments that indicate
// the value the expression wanted simply isn't there yet — typically a
// status field a controller hasn't filled in. These are retryable; the
// reconcile loop should requeue and try again later.
//
// Patterns that are *not* in this list (type errors, unknown overloads,
// etc.) are bugs in the user's expression and should fail loudly.
var celDataPendingPatterns = []string{
	"no such key",
	"no such field",
	"no such attribute",
	"index out of bounds",
}

// isCELDataPending classifies a CEL evaluation error as retryable (data
// not yet available) vs a hard user error.
func isCELDataPending(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, p := range celDataPendingPatterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}
