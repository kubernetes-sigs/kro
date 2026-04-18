// webhook_test.go provides a validating webhook server for fault injection.
//
// The webhook intercepts CREATE and UPDATE operations on ConfigMaps in
// namespaces matching a label selector. When reject mode is active, the
// webhook returns HTTP 500, causing the API server to return InternalError
// (5xx) to the controller — exercising the SystemError state machine path
// that was previously untested at integration level.
//
// Per 004-graph-reconciliation.md § Node States: "Server errors (5xx/timeout/
// network) → NodeSystemError. Retry with exponential backoff [1s, resyncInterval]."
package graphcontroller_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// faultWebhook is a validating webhook server for injecting API server faults.
// When rejectNames is non-empty, matching ConfigMap operations receive HTTP 500,
// causing the API server to return InternalError to the caller. When empty,
// all operations are accepted.
//
// Thread-safe: multiple goroutines (test + HTTP handler) access rejectNames
// concurrently.
type faultWebhook struct {
	mu          sync.RWMutex
	rejectNames map[string]bool // ConfigMap names to reject; empty = accept all

	server   *http.Server
	caBundle []byte
	addr     string
}

// Reject causes the webhook to return HTTP 500 for ConfigMaps with the given
// names. Additive — call multiple times to build the reject set.
func (fw *faultWebhook) Reject(names ...string) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	for _, name := range names {
		fw.rejectNames[name] = true
	}
}

// Accept removes names from the reject set. When the reject set is empty,
// all operations are accepted.
func (fw *faultWebhook) Accept(names ...string) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	for _, name := range names {
		delete(fw.rejectNames, name)
	}
}

// AcceptAll clears the reject set.
func (fw *faultWebhook) AcceptAll() {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	fw.rejectNames = make(map[string]bool)
}

func (fw *faultWebhook) shouldReject(name string) bool {
	fw.mu.RLock()
	defer fw.mu.RUnlock()
	return fw.rejectNames[name]
}

// startFaultWebhook creates a TLS webhook server on a random port. The server
// generates a self-signed CA and server cert, so the API server can verify the
// TLS connection via the caBundle in the ValidatingWebhookConfiguration.
//
// The server is stopped automatically when the test completes.
func startFaultWebhook(t *testing.T) *faultWebhook {
	t.Helper()

	// --- Generate self-signed CA ---
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"kro-test-ca"}},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	caCert, err := x509.ParseCertificate(caCertDER)
	require.NoError(t, err)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCertDER})

	// --- Generate server cert signed by CA ---
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{Organization: []string{"kro-test-webhook"}},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	serverCertDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	require.NoError(t, err)
	serverCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverCertDER})
	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	require.NoError(t, err)
	serverKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER})

	tlsCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	require.NoError(t, err)

	// --- Start TLS listener ---
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	fw := &faultWebhook{
		caBundle:    caPEM,
		addr:        ln.Addr().String(),
		rejectNames: make(map[string]bool),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/validate", fw.handleValidate)

	tlsLn := tls.NewListener(ln, &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
	})

	fw.server = &http.Server{Handler: mux}
	go fw.server.Serve(tlsLn) //nolint:errcheck

	t.Cleanup(func() {
		fw.server.Close()
	})

	return fw
}

func (fw *faultWebhook) handleValidate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	review := admissionv1.AdmissionReview{}
	if err := json.Unmarshal(body, &review); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Extract the resource name from the admission request.
	name := review.Request.Name
	if name == "" && review.Request.Object.Raw != nil {
		// For CREATE, the name might be in the object body.
		var obj map[string]any
		if err := json.Unmarshal(review.Request.Object.Raw, &obj); err == nil {
			if meta, ok := obj["metadata"].(map[string]any); ok {
				name, _ = meta["name"].(string)
			}
		}
	}

	if fw.shouldReject(name) {
		// Return HTTP 500 to trigger failurePolicy=Fail → InternalError (5xx)
		// on the client. Per errors.go: apierrors.IsInternalError → NodeSystemError.
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Accept the request.
	response := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Response: &admissionv1.AdmissionResponse{
			UID:     review.Request.UID,
			Allowed: true,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response) //nolint:errcheck
}

// registerFaultWebhook creates a ValidatingWebhookConfiguration that routes
// ConfigMap CREATE/UPDATE operations in matching namespaces to the fault
// webhook server. The configuration is deleted when the test completes.
//
// The nsLabelKey/nsLabelValue pair selects namespaces. Only ConfigMaps in
// namespaces with this label are intercepted — Graph and GraphRevision
// resources (experimental.kro.run group) are never affected.
func registerFaultWebhook(t *testing.T, c client.Client, webhookName string, fw *faultWebhook, nsLabelKey, nsLabelValue string) {
	t.Helper()

	fail := admissionregistrationv1.Fail
	none := admissionregistrationv1.SideEffectClassNone

	webhook := &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: webhookName},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{{
			Name: "fault.test.kro.run",
			ClientConfig: admissionregistrationv1.WebhookClientConfig{
				URL:      ptr.To(fmt.Sprintf("https://%s/validate", fw.addr)),
				CABundle: fw.caBundle,
			},
			Rules: []admissionregistrationv1.RuleWithOperations{{
				Operations: []admissionregistrationv1.OperationType{
					admissionregistrationv1.Create,
					admissionregistrationv1.Update,
				},
				Rule: admissionregistrationv1.Rule{
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"configmaps"},
				},
			}},
			FailurePolicy:           &fail,
			SideEffects:             &none,
			AdmissionReviewVersions: []string{"v1"},
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					nsLabelKey: nsLabelValue,
				},
			},
		}},
	}

	require.NoError(t, c.Create(ctx, webhook))
	t.Cleanup(func() {
		_ = c.Delete(ctx, webhook)
	})
}

// labelNamespace adds labels to an existing namespace. Used to opt namespaces
// into webhook fault injection.
func labelNamespace(t *testing.T, ns string, labels map[string]string) {
	t.Helper()
	namespace := &unstructured.Unstructured{}
	namespace.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: ns}, namespace))
	existing := namespace.GetLabels()
	if existing == nil {
		existing = make(map[string]string)
	}
	for k, v := range labels {
		existing[k] = v
	}
	namespace.SetLabels(existing)
	require.NoError(t, k8sClient.Update(ctx, namespace))
}
