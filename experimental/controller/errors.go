// errors.go classifies Kubernetes API errors into plan states.
//
// Client errors (4xx) → NodeError. Server errors (5xx/timeout/network) →
// NodeSystemError. Non-API errors (CEL, template) → NodeError.
// The plan state flows into the Graph's status condition, giving operators
// a clean signal for triage.
package graphcontroller

import (
	"context"
	"errors"
	"net"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// ErrPending indicates that CEL evaluation failed because required data
// is not yet available (e.g., a resource's status field hasn't been populated).
// This is a retryable condition — the controller should requeue and try again.
var ErrPending = errors.New("data pending")

// ErrEvaluation indicates that the error originates from a non-API operation:
// CEL evaluation, template rendering, JSON marshaling, or other deterministic
// local computation. Errors wrapped with this sentinel are classified as
// NodeError by classifyAPIError, even if their message text happens to contain
// network-like patterns (e.g., "unexpected EOF" from malformed JSON). Without
// this, the string-based network error pattern matcher would misclassify them
// as NodeSystemError, triggering 5-second retry for errors that can only
// resolve via propagation or spec change.
var ErrEvaluation = errors.New("evaluation error")

// ErrWaitingForReadiness indicates that a resource exists but hasn't satisfied
// its readyWhen conditions yet. Downstream resources should wait.
var ErrWaitingForReadiness = errors.New("waiting for readiness")

// ErrReadyWhenFailed indicates that a readyWhen expression failed to evaluate
// due to a permanent expression error (not data pending, not a transient
// condition). Per 001-graph.md: "readyWhen is a health signal — it does not
// gate downstream execution." The coordinator classifies this as NodeNotReady
// (not NodeError) so dependents proceed. The underlying error is preserved in
// the chain for logging and status reporting.
var ErrReadyWhenFailed = errors.New("readyWhen evaluation failed")

// ErrFieldConflict indicates that an SSA apply received a 409 Conflict because
// another actor has taken ownership of fields the controller manages. This is
// a permanent error for the resource until the external actor releases the
// field or the Graph spec changes to no longer write that field.
var ErrFieldConflict = errors.New("field conflict")

// celPendingPatterns are CEL error patterns that indicate data is not yet
// available (retryable). Other CEL errors are considered expression bugs.
var celPendingPatterns = []string{
	"no such key",
	"no such field",
	"no such attribute",
	"index out of bounds",
}

// isCELPending checks if a CEL error indicates data is pending.
func isCELPending(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, pattern := range celPendingPatterns {
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}

// isPending checks if an error indicates data is pending — either a CEL
// runtime error (string pattern match) or a wrapped ErrPending sentinel.
func isPending(err error) bool {
	return isCELPending(err) || errors.Is(err, ErrPending)
}

// apiErrorInfo holds the plan state and reason for an API error.
type apiErrorInfo struct {
	state  NodeState // NodeError (4xx) or NodeSystemError (5xx)
	reason string           // human-readable reason for status reporting
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
//   - Per 005-reconciliation.md § Node States: Definition nodes do not
//     produce SystemError (no API calls). CEL evaluation failures are Error.
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
	// changes), revision transition (user fixes the spec), or resync timer.
	// Per 005-reconciliation.md § Node States: Definition nodes produce
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

// isTransientError reports whether an error is transient — i.e., might
// succeed if retried without any input change. Context cancellation, API
// server responses (apierrors.StatusError), and network errors are
// transient. Everything else — compilation failures, CEL validation,
// cycle detection, parse errors — is deterministic: same input always
// produces the same failure.
//
// Used by Reconcile to decide whether to return an error to
// controller-runtime (triggering exponential backoff) or return nil
// (relying on watch events to re-enqueue when input changes).
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// In compilation paths, API errors are typically 404 (CRD not yet
	// registered) or 409 (conflict) — both transient. Deterministic
	// API errors (400, 422) would indicate a code bug, not user input.
	var statusErr *apierrors.StatusError
	if errors.As(err, &statusErr) {
		return true
	}
	return isNetworkError(err)
}
