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

package graph

import (
	"errors"
	"fmt"
)

// TerminalError indicates a problem the user must fix in the RGD.
// The controller should not retry — the RGD spec needs to change.
type TerminalError struct {
	Stage string
	Err   error
}

func (e *TerminalError) Error() string { return fmt.Sprintf("%s: %v", e.Stage, e.Err) }
func (e *TerminalError) Unwrap() error { return e.Err }

// RetriableError indicates a transient failure (e.g., API server unavailable).
// The controller should retry after backoff.
type RetriableError struct {
	Stage string
	Err   error
}

func (e *RetriableError) Error() string { return fmt.Sprintf("%s (retriable): %v", e.Stage, e.Err) }
func (e *RetriableError) Unwrap() error { return e.Err }

// IsTerminal reports whether err (or any error in its chain) is terminal.
func IsTerminal(err error) bool {
	var te *TerminalError
	return errors.As(err, &te)
}

// IsRetriable reports whether err (or any error in its chain) is retriable.
func IsRetriable(err error) bool {
	var re *RetriableError
	return errors.As(err, &re)
}

func terminal(stage string, err error) error  { return &TerminalError{Stage: stage, Err: err} }
func retriable(stage string, err error) error { return &RetriableError{Stage: stage, Err: err} }
func terminalf(stage, format string, a ...any) error {
	return terminal(stage, fmt.Errorf(format, a...))
}
