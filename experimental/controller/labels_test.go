package graphcontroller

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIdentityLabelKey(t *testing.T) {
	key := identityLabelKey("deploy", "my-app", "default")
	assert.Equal(t, "deploy.my-app.default.internal.kro.run/role", key)
}

func TestGenerationLabelKey(t *testing.T) {
	key := generationLabelKey("deploy", "my-app", "default")
	assert.Equal(t, "deploy.my-app.default.internal.kro.run/generation", key)
}

func TestParseIdentityLabel(t *testing.T) {
	tests := []struct {
		name      string
		key       string
		wantNode  string
		wantGraph string
		wantNS    string
		wantOK    bool
	}{
		{
			name:      "valid identity label",
			key:       "deploy.my-app.default.internal.kro.run/role",
			wantNode:  "deploy",
			wantGraph: "my-app",
			wantNS:    "default",
			wantOK:    true,
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
			name:   "missing segments",
			key:    "my-app.default.internal.kro.run/role",
			wantOK: false,
		},
		{
			name:   "empty node ID",
			key:    ".my-app.default.internal.kro.run/role",
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nodeID, graphName, namespace, ok := parseIdentityLabel(tc.key)
			assert.Equal(t, tc.wantOK, ok)
			if ok {
				assert.Equal(t, tc.wantNode, nodeID)
				assert.Equal(t, tc.wantGraph, graphName)
				assert.Equal(t, tc.wantNS, namespace)
			}
		})
	}
}

func TestIsGraphIdentityLabel(t *testing.T) {
	assert.True(t, isGraphIdentityLabel(
		"deploy.my-app.default.internal.kro.run/role", "my-app", "default"))
	assert.False(t, isGraphIdentityLabel(
		"deploy.other-app.default.internal.kro.run/role", "my-app", "default"))
	assert.False(t, isGraphIdentityLabel(
		"deploy.my-app.other-ns.internal.kro.run/role", "my-app", "default"))
	assert.False(t, isGraphIdentityLabel(
		"app.kubernetes.io/name", "my-app", "default"))
}

func TestHasOtherGraphIdentityLabel(t *testing.T) {
	// Resource with our label only — no conflict
	labels1 := map[string]string{
		"deploy.my-app.default.internal.kro.run/role": "owns",
	}
	_, found := hasOtherGraphIdentityLabel(labels1, "my-app", "default")
	assert.False(t, found)

	// Resource with another graph's label — conflict
	labels2 := map[string]string{
		"deploy.other-app.default.internal.kro.run/role": "owns",
	}
	otherGraph, found := hasOtherGraphIdentityLabel(labels2, "my-app", "default")
	assert.True(t, found)
	assert.Equal(t, "other-app", otherGraph)

	// Resource with both our label and another graph's label — conflict
	labels3 := map[string]string{
		"deploy.my-app.default.internal.kro.run/role":    "owns",
		"deploy.other-app.default.internal.kro.run/role": "contributes",
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

func TestSetIdentityLabels(t *testing.T) {
	labels := setIdentityLabels(nil, "deploy", "my-app", "default", "3", RoleOwns)
	assert.Equal(t, "owns", labels["deploy.my-app.default.internal.kro.run/role"])
	assert.Equal(t, "3", labels["deploy.my-app.default.internal.kro.run/generation"])
}
