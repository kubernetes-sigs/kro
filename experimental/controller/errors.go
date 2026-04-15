// errors.go classifies Kubernetes API errors into plan states.
//
// Client errors (4xx) → NodeError. Server errors (5xx/timeout/network) →
// NodeSystemError. Non-API errors (CEL, template) → NodeError.
// The plan state flows into the Graph's status condition, giving operators
// a clean signal for triage.
package graphcontroller

import (
	"errors"
	"net"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// apiErrorInfo holds the plan state and reason for an API error.
type apiErrorInfo struct {
	state  NodeState // NodeError (4xx) or NodeSystemError (5xx)
	reason string    // human-readable reason for status reporting
}

// classifyAPIError maps an error to a plan state and reason.
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
// Non-API errors (CEL evaluation, template rendering) → NodeError:
//   - Deterministic failures that retry cannot resolve.
//   - Per 004-graph-execution.md: "Definition nodes can be Ready, NotReady
//     (readyWhen unsatisfied), or Error (CEL evaluation failure). They cannot
//     be... SystemError (no API calls)."
//
// 404 and 409 are handled separately by callers (ErrPending, ErrFieldConflict).
func classifyAPIError(err error) apiErrorInfo {
	if err == nil {
		return apiErrorInfo{}
	}
	// Recognized client errors (4xx) → NodeError
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
	// Recognized server errors (5xx) → NodeSystemError
	if apierrors.IsInternalError(err) || apierrors.IsServiceUnavailable(err) ||
		apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsTooManyRequests(err) {
		return apiErrorInfo{state: NodeSystemError, reason: "ServerError"}
	}
	// Evaluation errors — deterministic failures from CEL, template rendering,
	// or marshaling. The ErrEvaluation sentinel is wrapped at the source
	// (toMap, evalString, reconcileDefinition) so the classifier can
	// distinguish "unexpected EOF in a JSON field" from "unexpected EOF on
	// the network." Without this check, the network pattern matcher below
	// would misclassify evaluation errors whose messages happen to contain
	// network-like substrings.
	if errors.Is(err, ErrEvaluation) {
		return apiErrorInfo{state: NodeError, reason: err.Error()}
	}
	// Network/infrastructure errors → NodeSystemError.
	// Raw Go network errors (*net.OpError, DNS failures, TLS handshake errors,
	// connection refused) are definitionally transient — the API server may
	// recover. Safe direction: retry at 5s.
	if isNetworkError(err) {
		return apiErrorInfo{state: NodeSystemError, reason: err.Error()}
	}
	// Default: non-API, non-network errors are deterministic failures
	// (CEL evaluation, template rendering, marshaling, etc.). These cannot
	// be resolved by retry — they resolve via propagation (upstream input
	// changes), revision transition (user fixes the spec), or drift timer.
	// Per 004-graph-execution.md § Node Evaluation: Definition nodes produce
	// Error on CEL failure, not SystemError.
	return apiErrorInfo{state: NodeError, reason: err.Error()}
}

// isNetworkError returns true if the error chain contains a network-level
// error (net.Error, net.OpError, etc.) that indicates transient infrastructure
// failure. These errors justify SystemError's 5s retry interval.
func isNetworkError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// Also check for common network error patterns in wrapped errors
	// that don't implement net.Error (e.g., "connection refused" from
	// non-net error wrappers).
	msg := err.Error()
	for _, pattern := range networkErrorPatterns {
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}

// networkErrorPatterns are error message substrings that indicate transient
// network/infrastructure failures. These supplement the net.Error interface
// check for errors that are wrapped without preserving the net.Error type.
var networkErrorPatterns = []string{
	"connection refused",
	"connection reset",
	"no such host",
	"i/o timeout",
	"TLS handshake",
	"unexpected EOF",
}
