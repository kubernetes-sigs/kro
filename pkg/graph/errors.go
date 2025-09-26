package graph

import "fmt"

// MultiError aggregates multiple errors into a single error value.
type MultiError struct {
	Errors []error
}

func (e *MultiError) Error() string {
	if e == nil || len(e.Errors) == 0 {
		return "no errors"
	}
	return fmt.Sprintf("%d error(s); first: %v", len(e.Errors), e.Errors[0])
}

// describes the category of a validation error.
type ValidationErrorType string

const (

	//  indicates a resource build/validation error
	ValidationErrorTypeResource ValidationErrorType = "resource"

	//  indicates a CEL expression error
	ValidationErrorTypeExpr ValidationErrorType = "expr"
	//  indicates a readyWhen expression error
	ValidationErrorTypeReadyWhen ValidationErrorType = "readyWhen"
	//  indicates an includeWhen expression error
	ValidationErrorTypeIncludeWhen ValidationErrorType = "includeWhen"
	//  indicates an apiVersion/kind (GVK) error for a resource
	ValidationErrorTypeGVK ValidationErrorType = "gvk"
	//  indicates a dependency graph extraction/validation error
	ValidationErrorTypeGraph ValidationErrorType = "graph"
)

// ValidationError represents validation error with extra context to help LSP map diagnostics to locations.
type ValidationError struct {
	Type       ValidationErrorType
	ResourceID string
	Expression string
	Path       string
	Err        error
}

func (e *ValidationError) Error() string {
	if e == nil {
		return "validation error"
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("%s error on resource %s", e.Type, e.ResourceID)
}

func NewValidationError(kind ValidationErrorType, resourceID, expression, path string, underlying error) *ValidationError {
	return &ValidationError{
		ResourceID: resourceID,
		Type:       kind,
		Expression: expression,
		Path:       path,
		Err:        underlying,
	}
}
