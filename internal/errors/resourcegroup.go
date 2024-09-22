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
