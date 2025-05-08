// Copyright 2025 The Kube Resource Orchestrator Authors
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

package requeue

import (
	"time"
)

const (
	DefaultRequeueAfterDuration time.Duration = 30 * time.Second
)

// None returns a new NoRequeue to instruct the ACK runtime to not requeue
// the processing item but to continue logging the error.
func None(err error) *NoRequeue {
	return &NoRequeue{
		err: err,
	}
}

// Needed returns a new RequeueNeeded to instruct the ACK runtime to requeue
// the processing item without been logged as error.
func Needed(err error) *RequeueNeeded {
	return &RequeueNeeded{
		err: err,
	}
}

// NeededAfter returns a new RequeueNeededAfter to instruct controller-runtime
// to requeue the processing item after specified duration without been logged
// as error.
func NeededAfter(
	err error,
	duration time.Duration,
) *RequeueNeededAfter {
	return &RequeueNeededAfter{
		RequeueNeeded{
			err: err,
		},
		duration,
	}
}

// NoRequeue instructs the ACK runtime to process an error, but not requeue the
// object that raised it. This should be used when there was a non-terminal
// error, but one that cannot be fixed by requeuing. e.g. a FieldExport failed
// because the source resource wasn't found.
type NoRequeue struct {
	err error
}

func (e *NoRequeue) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *NoRequeue) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// Ensure NoRequeue implements the error interface
var _ error = &NoRequeue{}

// RequeueNeeded instructs the ACK runtime to requeue the processing item
// without been logged as error.  This should be used when a "error condition"
// occurrence is sort of expected and can be resolved by retry.  e.g. a
// dependency haven't been fulfilled yet.
type RequeueNeeded struct {
	err error
}

func (e *RequeueNeeded) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *RequeueNeeded) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// Ensure RequeueNeeded implements the error interface
var _ error = &RequeueNeeded{}

// RequeueNeededAfter instructs the ACK runtime to requeue the processing item
// after specified duration without been logged as error. This should be used
// when an "error condition" occurrence is sort of expected and can be resolved
// by retry.  E.g., a dependency hasn't been fulfilled yet, and expected it to
// be fulfilled after duration.  Note: use this with care, a simple wait might
// suit your use case better.
type RequeueNeededAfter struct {
	RequeueNeeded
	duration time.Duration
}

func (e *RequeueNeededAfter) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *RequeueNeededAfter) Duration() time.Duration {
	if e == nil {
		return time.Duration(0) * time.Second
	}
	return e.duration
}

func (e *RequeueNeededAfter) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// Ensure RequeueNeededAfter implements the error interface
var _ error = &RequeueNeededAfter{}
