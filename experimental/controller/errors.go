// errors.go classifies Kubernetes API errors into plan states.
//
// Client errors (4xx) → NodeError. Server errors (5xx/timeout/network) →
// NodeSystemError. The plan state flows into the Graph's status condition,
// giving operators a clean signal for triage.
package graphcontroller

import (
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// apiErrorInfo holds the plan state and reason for an API error.
type apiErrorInfo struct {
	state  NodeState // NodeError (4xx) or NodeSystemError (5xx)
	reason string    // human-readable reason for status reporting
}

// classifyAPIError maps a Kubernetes API error to a plan state and reason.
//
// Client errors (4xx) → NodeError:
//   - 400 Bad Request → "BadRequest" or "AdmissionDenied"
//   - 401 Unauthorized → "Unauthorized"
//   - 403 Forbidden → "Forbidden"
//   - 422 Unprocessable → "ValidationFailed"
//
// Server errors (5xx/timeout/network) → NodeSystemError:
//   - reason is the raw error message
//
// 404 and 409 are handled separately by callers (ErrDataPending, ErrFieldConflict).
func classifyAPIError(err error) apiErrorInfo {
	if err == nil {
		return apiErrorInfo{}
	}
	if apierrors.IsForbidden(err) {
		return apiErrorInfo{state: NodeError, reason: "Forbidden"}
	}
	if apierrors.IsUnauthorized(err) {
		return apiErrorInfo{state: NodeError, reason: "Unauthorized"}
	}
	if apierrors.IsInvalid(err) {
		return apiErrorInfo{state: NodeError, reason: "ValidationFailed"}
	}
	if apierrors.IsBadRequest(err) {
		if strings.Contains(err.Error(), "admission") {
			return apiErrorInfo{state: NodeError, reason: "AdmissionDenied"}
		}
		return apiErrorInfo{state: NodeError, reason: "BadRequest"}
	}
	// Server errors (5xx) — positively identified
	if apierrors.IsInternalError(err) || apierrors.IsServiceUnavailable(err) ||
		apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsTooManyRequests(err) {
		return apiErrorInfo{state: NodeSystemError, reason: "ServerError"}
	}
	// Default: unrecognized errors are client errors (NodeError).
	// Server errors are positively identified above.
	return apiErrorInfo{state: NodeError, reason: err.Error()}
}
