package stdlib

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// Apply installs the stdlib resources with retry. Some resources
// reference CRDs that don't exist yet — they fail on first attempt
// and succeed once the Graph controller creates the CRDs.
func Apply(ctx context.Context, log logr.Logger, cfg *rest.Config) error {
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	// Ensure kro-system namespace exists.
	nsGVR := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	nsData, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": "kro-system"},
	})
	if _, err := dc.Resource(nsGVR).Patch(ctx, "kro-system", types.ApplyPatchType, nsData,
		metav1.PatchOptions{FieldManager: "graph-controller", Force: ptr(true)}); err != nil {
		return fmt.Errorf("ensuring kro-system namespace: %w", err)
	}

	resources, err := loadResources()
	if err != nil {
		return err
	}

	pending := resources
	for attempt := 0; attempt < 30 && len(pending) > 0; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
		var failed []resource
		for _, r := range pending {
			if err := apply(ctx, dc, r); err != nil {
				if apierrors.IsNotFound(err) || strings.Contains(err.Error(), "the server could not find the requested resource") {
					failed = append(failed, r)
					if attempt == 0 {
						log.Info("waiting for CRD", "resource", r.name)
					}
				} else {
					return fmt.Errorf("applying %s: %w", r.name, err)
				}
			} else {
				log.Info("applied", "resource", r.name)
			}
		}
		pending = failed
	}
	if len(pending) > 0 {
		names := make([]string, len(pending))
		for i, r := range pending {
			names[i] = r.name
		}
		return fmt.Errorf("timed out: %v", names)
	}
	return nil
}

type resource struct {
	name string
	data []byte
	gvr  unstructured.Unstructured
}

func apply(ctx context.Context, dc dynamic.Interface, r resource) error {
	gvk := r.gvr.GroupVersionKind()
	gvr := gvk.GroupVersion().WithResource(plural(gvk.Kind))
	ns := r.gvr.GetNamespace()

	var rc dynamic.ResourceInterface = dc.Resource(gvr)
	if ns != "" {
		rc = dc.Resource(gvr).Namespace(ns)
	}
	_, err := rc.Patch(ctx, r.gvr.GetName(), types.ApplyPatchType, r.data,
		metav1.PatchOptions{FieldManager: "graph-controller", Force: ptr(true)})
	return err
}

// fileOrder defines the bootstrap sequence. Kind is the bootstrap root
// (raw Graph). All others are Kinds that depend on the Kind CRD existing.
var fileOrder = []string{
	"kind.yaml",
	"decorator.yaml",
	"singleton.yaml",
	"rgd.yaml",
}

func loadResources() ([]resource, error) {
	var out []resource
	for _, name := range fileOrder {
		raw, err := fs.ReadFile(Resources, name)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
		for _, doc := range bytes.Split(raw, []byte("\n---")) {
			doc = bytes.TrimSpace(doc)
			if len(doc) == 0 {
				continue
			}
			obj := &unstructured.Unstructured{}
			if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), len(doc)).Decode(obj); err != nil {
				return nil, err
			}
			if obj.GetKind() == "" {
				continue
			}
			out = append(out, resource{
				name: fmt.Sprintf("%s/%s", obj.GetKind(), obj.GetName()),
				data: doc,
				gvr:  *obj,
			})
		}
	}
	return out, nil
}

func plural(kind string) string {
	switch kind {
	case "Graph":
		return "graphs"
	case "Kind":
		return "kinds"
	case "Decorator":
		return "decorators"
	case "Singleton":
		return "singletons"
	default:
		panic("stdlib: unknown kind " + kind)
	}
}

func ptr[T any](v T) *T { return &v }
