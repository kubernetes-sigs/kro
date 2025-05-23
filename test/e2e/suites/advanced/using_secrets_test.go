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

package advanced

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kro-run/kro/test/e2e/framework"
)

func TestSecretsInResources(t *testing.T) {

	tc, err := framework.NewTestCase(t, f, "secrets-in-resources")
	require.NoError(t, err)

	// Run the test
	// Option 1: Run all steps automatically using the manifests to verify
	tc.RunTest(t, func(ctx context.Context) error {
		return tc.RunAllSteps(ctx)
	})

	// Option 2: Run steps with custom logic
	/*
		tc.RunTest(t, func(ctx context.Context) error {
			// install the rgd
			if err := tc.RunStep(ctx, "step0"); err != nil {
				return err
			}

			// create an instance of the rgd
			if err := tc.RunStep(ctx, "step1"); err != nil {
				return err
			}
			// Wait for deployment to be created
			err = tc.WaitForResource(ctx,
				schema.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "Deployment",
				},
				"test-instance",
				tc.Namespace,
				func(obj *unstructured.Unstructured) bool {
					replicas, found, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas")
					return found && replicas == 2
				},
			)
			if err != nil {
				return err
			}
			// update the instance
			if err := tc.RunStep(ctx, "step2"); err != nil {
				return err
			}

			// Wait for deployment to be created
			err = tc.WaitForResource(ctx,
				schema.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "Deployment",
				},
				"test-instance",
				tc.Namespace,
				func(obj *unstructured.Unstructured) bool {
					replicas, found, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas")
					return found && replicas == 3
				},
			)
			if err != nil {
				return err
			}
			return nil
		})
	*/
}
