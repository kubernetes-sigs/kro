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
	"fmt"
	"os"
	"testing"

	"github.com/kro-run/kro/test/e2e/framework"
)

var (
	f *framework.Framework
)

func TestMain(m *testing.M) {
	// Create framework
	var err error

	// Connect to the cluster (or create one)
	// Option 1: This uses the ENV provided kubeconfig instead of reading standard paths
	/*
		kubeconfig := os.Getenv("TEST_KUBECONFIG")
		if kubeconfig == "" {
			t.Skip("TEST_KUBECONFIG not set")
		}
		options := framework.WithKubeconfig(kubeconfig)
	*/

	// Option 2: Create a kind cluster. we dont support local image build yet.
	/*
		framework.WithKindCluster(
		framework.WithName("kro-basic-test"),
		framework.WithWaitTimeout(5*time.Minute),
	*/

	// Option 3: Do nothing. kubeconfig is loaded from standard path
	f, err = framework.New()
	if err != nil {
		fmt.Printf("failed to create framework: %v", err)
		os.Exit(1)
	}

	// Setup framework
	if err := f.Setup(context.Background()); err != nil {
		fmt.Printf("failed to setup framework: %v", err)
		os.Exit(1)
	}

	// Execute the tests
	code := m.Run()

	// Teardown framework
	if err := f.Teardown(context.Background()); err != nil {
		fmt.Printf("failed to teardown framework: %v", err)
		os.Exit(1)
	}

	// Exit with the test code
	os.Exit(code)
}
