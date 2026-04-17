package graphcontroller

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIdentityLabelKey(t *testing.T) {
	key := identityLabelKey("deploy", "my-app", "default")
	assert.Equal(t, "deploy.my-app.default.internal.kro.run/type", key)
}

func TestGenerationLabelKey(t *testing.T) {
	key := generationLabelKey("deploy", "my-app", "default")
	assert.Equal(t, "deploy.my-app.default.internal.kro.run/generation", key)
}

func TestParseNodeIDFromLabel(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		wantNode string
		wantOK   bool
	}{
		{
			name:     "regular node identity label",
			key:      "deploy.my-app.default.internal.kro.run/type",
			wantNode: "deploy",
			wantOK:   true,
		},
		{
			name:     "forEach child identity label",
			key:      "policies.default-deny.ns-a.networkpolicy.networking.k8s.io.mygraph.default.internal.kro.run/type",
			wantNode: "policies",
			wantOK:   true,
		},
		{
			name:   "not an identity label",
			key:    "app.kubernetes.io/name",
			wantOK: false,
		},
		{
			name:   "generation label (wrong suffix)",
			key:    "deploy.my-app.default.internal.kro.run/generation",
			wantOK: false,
		},
		{
			name:   "empty node ID (leading dot)",
			key:    ".my-app.default.internal.kro.run/type",
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nodeID, ok := parseNodeIDFromLabel(tc.key)
			assert.Equal(t, tc.wantOK, ok)
			if ok {
				assert.Equal(t, tc.wantNode, nodeID)
			}
		})
	}
}

func TestGraphNameFromLabel(t *testing.T) {
	tests := []struct {
		name      string
		key       string
		wantGraph string
	}{
		{
			name:      "regular node identity label",
			key:       "deploy.my-app.default.internal.kro.run/type",
			wantGraph: "my-app",
		},
		{
			name:      "forEach child identity label",
			key:       "policies.default-deny.ns-a.networkpolicy.networking.k8s.io.mygraph.default.internal.kro.run/type",
			wantGraph: "mygraph",
		},
		{
			name:      "forEach child without group",
			key:       "configs.my-cm.default.configmap.mygraph.default.internal.kro.run/type",
			wantGraph: "mygraph",
		},
		{
			name:      "not an identity label",
			key:       "app.kubernetes.io/name",
			wantGraph: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := graphNameFromLabel(tc.key)
			assert.Equal(t, tc.wantGraph, got)
		})
	}
}

func TestIsGraphIdentityLabel(t *testing.T) {
	assert.True(t, isGraphIdentityLabel(
		"deploy.my-app.default.internal.kro.run/type", "my-app", "default"))
	assert.False(t, isGraphIdentityLabel(
		"deploy.other-app.default.internal.kro.run/type", "my-app", "default"))
	assert.False(t, isGraphIdentityLabel(
		"deploy.my-app.other-ns.internal.kro.run/type", "my-app", "default"))
	assert.False(t, isGraphIdentityLabel(
		"app.kubernetes.io/name", "my-app", "default"))
}

func TestHasOtherGraphIdentityLabel(t *testing.T) {
	// Resource with our label only — no conflict
	labels1 := map[string]string{
		"deploy.my-app.default.internal.kro.run/type": "own",
	}
	_, found := hasOtherGraphIdentityLabel(labels1, "my-app", "default")
	assert.False(t, found)

	// Resource with another graph's label — conflict
	labels2 := map[string]string{
		"deploy.other-app.default.internal.kro.run/type": "own",
	}
	otherGraph, found := hasOtherGraphIdentityLabel(labels2, "my-app", "default")
	assert.True(t, found)
	assert.Equal(t, "other-app", otherGraph)

	// Resource with both our label and another graph's label — conflict
	labels3 := map[string]string{
		"deploy.my-app.default.internal.kro.run/type":    "own",
		"deploy.other-app.default.internal.kro.run/type": "contribute",
	}
	otherGraph, found = hasOtherGraphIdentityLabel(labels3, "my-app", "default")
	assert.True(t, found)
	assert.Equal(t, "other-app", otherGraph)

	// No identity labels at all
	labels4 := map[string]string{
		"app": "my-app",
	}
	_, found = hasOtherGraphIdentityLabel(labels4, "my-app", "default")
	assert.False(t, found)
}

// Regression: externally-authored identity labels with mixed case (e.g., a
// human adding a label through kubectl edit, or a label that survived an
// upgrade from a pre-lowercase-normalized version) must still trigger the
// conflict check. isGraphIdentityLabel and isIdentityLabel both lowercase
// before compare; hasOtherGraphIdentityLabel must match that invariant.
func TestHasOtherGraphIdentityLabel_RegressionMixedCase(t *testing.T) {
	mixedCase := map[string]string{
		// Identity label key with uppercase characters — would never be
		// stamped by the controller (stamping enforces lowercase), but
		// could be present from an edit or older stamping.
		"Deploy.Other-App.Default.internal.kro.run/type": "own",
	}
	otherGraph, found := hasOtherGraphIdentityLabel(mixedCase, "my-app", "default")
	assert.True(t, found, "mixed-case identity label from another graph must be detected")
	assert.Equal(t, "other-app", otherGraph, "graph name should be lowercased in the returned value")
}

func TestSetIdentityLabels(t *testing.T) {
	labels := setIdentityLabels(nil, "deploy", "my-app", "default", "3", NodeTypeOwn)
	assert.Equal(t, "own", labels["deploy.my-app.default.internal.kro.run/type"])
	assert.Equal(t, "3", labels["deploy.my-app.default.internal.kro.run/generation"])
}

// TestIdentityLabelKeyProducesValidSubdomain verifies the invariant that
// validateIdentityLabelKey rejects node IDs producing invalid DNS-1123
// subdomain label prefixes. Kubernetes requires the prefix portion of a
// qualified label key (everything before the /) to be a valid DNS subdomain.
// Node IDs are embedded directly in that prefix.
func TestIdentityLabelKeyProducesValidSubdomain(t *testing.T) {
	tests := []struct {
		name    string
		nodeID  string
		graph   string
		ns      string
		wantErr bool
	}{
		{
			name:    "simple alphanumeric",
			nodeID:  "deploy",
			graph:   "myapp",
			ns:      "default",
			wantErr: false,
		},
		{
			name:    "underscore in node ID",
			nodeID:  "prstatus_test_app",
			graph:   "myapp",
			ns:      "default",
			wantErr: true,
		},
		{
			name:    "single underscore",
			nodeID:  "foo_bar",
			graph:   "g",
			ns:      "default",
			wantErr: true,
		},
		{
			name:    "space in node ID",
			nodeID:  "foo bar",
			graph:   "g",
			ns:      "default",
			wantErr: true,
		},
		{
			name:    "trailing dot in node ID",
			nodeID:  "foo.",
			graph:   "g",
			ns:      "default",
			wantErr: true,
		},
		{
			name:    "mixed case lowered is still valid",
			nodeID:  "Deploy",
			graph:   "MyApp",
			ns:      "Default",
			wantErr: false,
		},
		{
			name:    "single character",
			nodeID:  "a",
			graph:   "g",
			ns:      "default",
			wantErr: false,
		},
		{
			name:    "multi-segment all valid",
			nodeID:  "promotionstep",
			graph:   "myapp",
			ns:      "production",
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateIdentityLabelKey(tc.nodeID, tc.graph, tc.ns)
			if tc.wantErr {
				assert.Error(t, err,
					"node ID %q should be rejected — produces invalid DNS subdomain label prefix",
					tc.nodeID)
			} else {
				assert.NoError(t, err,
					"node ID %q should be accepted — produces valid DNS subdomain label prefix",
					tc.nodeID)
			}
		})
	}
}

// TestLabelSafeGraphName verifies that graph names exceeding the
// Kubernetes 63-byte label-value limit are deterministically truncated
// and suffixed with a hash so distinct names produce distinct labels.
func TestLabelSafeGraphName(t *testing.T) {
	tests := []struct {
		name      string
		graphName string
	}{
		{name: "short name passes through", graphName: "my-graph"},
		{name: "exactly 63 bytes passes through", graphName: strings.Repeat("a", 63)},
		{name: "over 63 bytes is truncated", graphName: strings.Repeat("a", 100)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := labelSafeGraphName(tc.graphName)
			assert.LessOrEqual(t, len(got), maxLabelValueLength,
				"result must fit in a Kubernetes label value")
			if len(tc.graphName) <= maxLabelValueLength {
				assert.Equal(t, tc.graphName, got, "short names pass through unchanged")
			}
		})
	}

	// Different long names must not collide after truncation.
	a := labelSafeGraphName("test-instance-resource-reconcile-reactive-resourcegraphdefinition")
	b := labelSafeGraphName("test-instance-resource-reconcile-alternate-resourcegraphdefinition")
	assert.NotEqual(t, a, b, "distinct long names must produce distinct labels")

	// Determinism: same input produces same output.
	assert.Equal(t, a, labelSafeGraphName("test-instance-resource-reconcile-reactive-resourcegraphdefinition"))
}
