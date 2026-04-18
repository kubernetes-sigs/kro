//go:build graphcompat

// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package core_test runs the upstream kro integration tests against the
// experimental Graph controller. Test files are symlinked from
// test/integration/suites/core/ — this file replaces the upstream
// setup_test.go to start the Graph controller as a subprocess.
//
// Usage:
//
//	# Create symlinks (once)
//	for f in test/integration/suites/core/*_test.go; do
//	  [ "$(basename $f)" != "setup_test.go" ] && ln -sf "../../../$f" \
//	    experimental/test/rgd_compat/
//	done
//	# Run
//	go test -tags graphcompat -v -timeout 5m \
//	  ./experimental/test/rgd_compat/ --ginkgo.focus-file=lifecycle_test.go
package core_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	internalv1alpha1 "github.com/kubernetes-sigs/kro/api/internal.kro.run/v1alpha1"
	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/experimental/test/testenv"
	"github.com/kubernetes-sigs/kro/test/integration/environment"
)

// env is the single shared variable that all upstream test files reference.
// They only access env.Client.
var env *environment.Environment

var testEnvironment *testenv.Environment

func TestCore(t *testing.T) {
	RegisterFailHandler(Fail)

	BeforeSuite(func() {
		logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

		moduleRoot := testenv.FindModuleRoot()

		// Start envtest + subprocess.
		testEnvironment = &testenv.Environment{
			CRDDirectoryPaths: []string{
				filepath.Join(moduleRoot, "test", "integration", "crds", "ack-ec2-controller"),
				filepath.Join(moduleRoot, "test", "integration", "crds", "ack-iam-controller"),
				filepath.Join(moduleRoot, "test", "integration", "crds", "ack-eks-controller"),
			},
		}
		cfg, err := testEnvironment.Start()
		Expect(err).NotTo(HaveOccurred())
		GinkgoWriter.Println("controller healthy, type tower bootstrapping...")

		// Register kro schemes for typed client operations.
		Expect(krov1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())
		Expect(internalv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())

		// Create raw client and wait for the RGD CRD — signals the full
		// stdlib type tower has materialized.
		rawClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		Expect(waitForCRD(rawClient, "resourcegraphdefinitions.kro.run")).To(Succeed())
		GinkgoWriter.Println("RGD CRD established — ready")

		// Wrap client with metadata filtering and populate env.
		env = &environment.Environment{
			Client: &metadataFilteringClient{Client: rawClient},
		}
	})

	AfterSuite(func() {
		if testEnvironment != nil {
			testEnvironment.Stop()
		}
	})

	// Allow the Makefile to restrict which compat files run via env var,
	// so the compat suite can share a single `go test ./experimental/...`
	// invocation with the unit and integration packages (parallel via -p).
	// CLI --ginkgo.focus-file still works for single-file iteration.
	suiteConfig, reporterConfig := GinkgoConfiguration()
	if files := os.Getenv("COMPAT_FOCUS_FILES"); files != "" {
		suiteConfig.FocusFiles = append(suiteConfig.FocusFiles, strings.Fields(files)...)
	}
	RunSpecs(t, "Graph Compat Suite", suiteConfig, reporterConfig)
}

// toRawExtension converts a value to runtime.RawExtension via JSON.
// Used by upstream test files (crd_test.go, recover_test.go).
func toRawExtension(v interface{}) runtime.RawExtension {
	rawJSON, err := json.Marshal(v)
	Expect(err).NotTo(HaveOccurred())
	return runtime.RawExtension{Raw: rawJSON}
}

// ---------------------------------------------------------------------------
// Metadata filtering client
// ---------------------------------------------------------------------------

// metadataFilteringClient strips internal.kro.run/* annotations and labels
// from objects returned by Get and List. These are implementation details
// that upstream tests should not see.
type metadataFilteringClient struct {
	client.Client
}

func (c *metadataFilteringClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	stripInternalMetadata(obj)
	return nil
}

func (c *metadataFilteringClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil
	}
	for _, item := range items {
		if obj, ok := item.(client.Object); ok {
			stripInternalMetadata(obj)
		}
	}
	return nil
}

func stripInternalMetadata(obj client.Object) {
	if annotations := obj.GetAnnotations(); len(annotations) > 0 {
		filtered := make(map[string]string, len(annotations))
		for k, v := range annotations {
			if !strings.Contains(k, "internal.kro.run") {
				filtered[k] = v
			}
		}
		if len(filtered) == 0 {
			filtered = nil
		}
		obj.SetAnnotations(filtered)
	}
	if labels := obj.GetLabels(); len(labels) > 0 {
		filtered := make(map[string]string, len(labels))
		for k, v := range labels {
			if !strings.Contains(k, "internal.kro.run") {
				filtered[k] = v
			}
		}
		if len(filtered) == 0 {
			filtered = nil
		}
		obj.SetLabels(filtered)
	}
}

// ---------------------------------------------------------------------------
// CRD readiness
// ---------------------------------------------------------------------------

func waitForCRD(c client.Client, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	return wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := c.Get(ctx, types.NamespacedName{Name: name}, crd); err != nil {
			return false, nil
		}
		for _, cond := range crd.Status.Conditions {
			if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
}
