// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package basic

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kro-run/kro/test/e2e/framework"
)

// TestAutoscaledDeploymentRGD tests a ResourceGraphDefinition that creates multiple dependent resources:
// - A ConfigMap containing application configuration
// - A Service for exposing the application
// - A Deployment that uses the ConfigMap and is exposed by the Service
// - A HorizontalPodAutoscaler for scaling the Deployment
//
// The test verifies:
// 1. Initial creation of all resources with correct dependencies
// 2. Updates to the ConfigMap trigger Deployment updates
// 3. Updates to replicas are respected by HPA
// 4. Deletion of resources in correct order
func TestAutoscaledDeploymentRGD(t *testing.T) {
	tc, err := framework.NewTestCase(t, f, "autoscaled-deployment-rgd")
	require.NoError(t, err)
	tc = tc.WithTimeout(3 * time.Minute)

	tc.RunTest(t, func(ctx context.Context) error {
		return tc.RunAllSteps(ctx)
	})
}
