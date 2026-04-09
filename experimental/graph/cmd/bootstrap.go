package main

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kubernetes-sigs/kro/experimental/graph/crds"
)

// bootstrap applies the embedded CRD manifests via server-side apply and waits
// for them to become established. This makes the binary self-provisioning — a
// single `go run ./experimental/graph/cmd/ --bootstrap` sets up everything the
// controller needs to run.
func bootstrap(ctx context.Context, cfg *rest.Config) error {
	log := ctrl.Log.WithName("bootstrap")

	cs, err := apiextensionsclient.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating apiextensions client: %w", err)
	}

	entries, err := fs.ReadDir(crds.YAMLs, ".")
	if err != nil {
		return fmt.Errorf("reading embedded CRDs: %w", err)
	}

	var crdNames []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := fs.ReadFile(crds.YAMLs, entry.Name())
		if err != nil {
			return fmt.Errorf("reading %s: %w", entry.Name(), err)
		}

		// Decode just to extract the name for logging and wait.
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), len(data)).Decode(crd); err != nil {
			return fmt.Errorf("decoding %s: %w", entry.Name(), err)
		}

		log.Info("applying CRD", "name", crd.Name)
		if _, err := cs.ApiextensionsV1().CustomResourceDefinitions().Patch(
			ctx, crd.Name, types.ApplyPatchType, data,
			metav1.PatchOptions{FieldManager: "graph-controller", Force: ptr(true)},
		); err != nil {
			return fmt.Errorf("applying CRD %s: %w", crd.Name, err)
		}
		crdNames = append(crdNames, crd.Name)
	}

	// Wait for all CRDs to be established.
	for _, name := range crdNames {
		log.Info("waiting for CRD to be established", "name", name)
		if err := waitForEstablished(ctx, cs, name); err != nil {
			return err
		}
	}

	log.Info("bootstrap complete", "crds", crdNames)
	return nil
}

func waitForEstablished(ctx context.Context, cs apiextensionsclient.Interface, name string) error {
	err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		crd, err := cs.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		for _, cond := range crd.Status.Conditions {
			if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("timed out waiting for CRD %s to become Established", name)
	}
	return nil
}

func ptr[T any](v T) *T { return &v }
