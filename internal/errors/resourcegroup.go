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

type ReconcileGraphError struct {
	err error
}

func (e *ReconcileGraphError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *ReconcileGraphError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

type ReconcileMicroControllerError struct {
	err error
}

func (e *ReconcileMicroControllerError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *ReconcileMicroControllerError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

type ReconcileCRDError struct {
	err error
}

func (e *ReconcileCRDError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *ReconcileCRDError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

type ProcessCRDError struct {
	err error
}

func (e *ProcessCRDError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *ProcessCRDError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func NewReconcileGraphError(err error) *ReconcileGraphError {
	return &ReconcileGraphError{err: err}
}

func NewReconcileMicroControllerError(err error) *ReconcileMicroControllerError {
	return &ReconcileMicroControllerError{err: err}
}

func NewReconcileCRDError(err error) *ReconcileCRDError {
	return &ReconcileCRDError{err: err}
}

func NewProcessCRDError(err error) *ProcessCRDError {
	return &ProcessCRDError{err: err}
}
