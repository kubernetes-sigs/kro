// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.
package errors

import "fmt"

// BaseError is a common structure for all custom errors
type BaseError struct {
	ErrType string
	Err     error
}

func (e *BaseError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %v", e.ErrType, e.Err)
}

func (e *BaseError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Custom error types
type (
	ReconcileGraphError struct {
		BaseError
	}

	ReconcileMicroControllerError struct {
		BaseError
	}

	ReconcileCRDError struct {
		BaseError
	}
)

// Constructor functions
func NewReconcileGraphError(err error) *ReconcileGraphError {
	return &ReconcileGraphError{BaseError{ErrType: "ReconcileGraphError", Err: err}}
}

func NewReconcileMicroControllerError(err error) *ReconcileMicroControllerError {
	return &ReconcileMicroControllerError{BaseError{ErrType: "ReconcileMicroControllerError", Err: err}}
}

func NewReconcileCRDError(err error) *ReconcileCRDError {
	return &ReconcileCRDError{BaseError{ErrType: "ReconcileCRDError", Err: err}}
}
