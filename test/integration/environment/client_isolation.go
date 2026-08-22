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

package environment

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// baseGroup is the real API group kro serves instances under (kro.run).
const baseGroup = krov1alpha1.KRODomainName

const (
	rgdKind   = "ResourceGraphDefinition"
	graphKind = "Graph"
)

// groupIsolatingClient virtualizes the kro.run API group so that specs running
// on different Ginkgo parallel processes against a SINGLE shared apiserver do
// not collide on the cluster-scoped CRDs that ResourceGraphDefinitions generate.
//
// Two specs that reuse the same schema kind (e.g. "TestValidation") would
// otherwise both map to the same cluster-scoped CRD (testvalidations.kro.run),
// and one spec's cleanup would delete the CRD out from under the other. To
// avoid that, each parallel process is assigned a distinct group derived from
// its Ginkgo process number, e.g. "p2.kro.run".
//
// The following base-group references are rewritten to the per-process group on
// their way to the apiserver, and restored on the way back:
//
//   - ResourceGraphDefinition.spec.schema.group (so the generated CRD lands in
//     the per-process group);
//   - kro.run/<v> apiVersions embedded inside RGD resource templates and
//     externalRef definitions (so cross-RGD references resolve to the
//     per-process CRDs);
//   - the GVK group of kro.run instance objects (unstructured);
//   - the name of a generated CRD fetched by name via Get (e.g.
//     "testresources.kro.run" -> "testresources.p2.kro.run").
//
// User-authored CustomResourceDefinition objects (which specs create directly
// and reference by name through instance data) are deliberately left untouched:
// they are created and resolved under their real group.
type groupIsolatingClient struct {
	client.Client
	procGroup string // e.g. "p2.kro.run"

	// knownKinds records the schema kinds of every ResourceGraphDefinition
	// created through this client. A kro.run reference (in an RGD resource
	// template or externalRef) is only rewritten to the per-process group when
	// its kind is a known RGD-generated kind; references to user-created CRDs
	// (which are never RGD schemas) are left at the real kro.run group.
	mu         sync.Mutex
	knownKinds map[string]bool
}

// NewGroupIsolatingClient wraps c so that all kro.run group references are
// virtualized under a per-process group "<token>.kro.run" (e.g. token "p2").
// If token is empty the client is returned unchanged.
func NewGroupIsolatingClient(c client.Client, token string) client.Client {
	if token == "" {
		return c
	}
	return &groupIsolatingClient{
		Client:     c,
		procGroup:  token + "." + baseGroup,
		knownKinds: map[string]bool{},
	}
}

func (g *groupIsolatingClient) recordKind(kind string) {
	if kind == "" {
		return
	}
	g.mu.Lock()
	g.knownKinds[kind] = true
	g.mu.Unlock()
}

func (g *groupIsolatingClient) isKnownKind(kind string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.knownKinds[kind]
}

// toProc rewrites base-group references on an object to the per-process group
// before the object is sent to the apiserver.
func (g *groupIsolatingClient) toProc(obj client.Object) {
	switch o := obj.(type) {
	case *krov1alpha1.ResourceGraphDefinition:
		if o.Spec.Schema != nil {
			g.recordKind(o.Spec.Schema.Kind)
			if o.Spec.Schema.Group == "" {
				o.Spec.Schema.Group = g.procGroup
			}
		}
		for _, r := range o.Spec.Resources {
			if r == nil {
				continue
			}
			if r.ExternalRef != nil && g.isKnownKind(r.ExternalRef.Kind) {
				r.ExternalRef.APIVersion = rewriteAPIVersionGroup(r.ExternalRef.APIVersion, baseGroup, g.procGroup)
			}
			if len(r.Template.Raw) > 0 {
				if raw, ok := g.rewriteRawTemplateGroup(r.Template.Raw); ok {
					r.Template.Raw = raw
					r.Template.Object = nil
				}
			}
		}
	case *unstructured.Unstructured:
		gvk := o.GroupVersionKind()
		if gvk.Group != baseGroup {
			return
		}
		if gvk.Kind == rgdKind {
			if kind, found, _ := unstructured.NestedString(o.Object, "spec", "schema", "kind"); found {
				g.recordKind(kind)
			}
			if grp, found, _ := unstructured.NestedString(o.Object, "spec", "schema", "group"); !found || grp == "" {
				_ = unstructured.SetNestedField(o.Object, g.procGroup, "spec", "schema", "group")
			}
			if resources, found, _ := unstructured.NestedSlice(o.Object, "spec", "resources"); found {
				g.rewriteGroupInValue(resources)
				_ = unstructured.SetNestedSlice(o.Object, resources, "spec", "resources")
			}
			return
		}
	}
}

// fromProc reverses the per-process CRD rewrite on an object returned from the
// apiserver, so that assertions written against the base group continue to
// hold. Instances are handled entirely by instanceFallback and are left at the
// group the controller actually used.
func (g *groupIsolatingClient) fromProc(obj client.Object) {
	if o, ok := obj.(*apiextensionsv1.CustomResourceDefinition); ok {
		o.Name = rewriteCRDNameSuffix(o.Name, g.procGroup, baseGroup)
		if o.Spec.Group == g.procGroup {
			o.Spec.Group = baseGroup
		}
	}
	// kro stamps the instance's group onto managed resources via the
	// instance-group label. Restore its value so specs that assert the literal
	// kro.run group on child resources continue to hold.
	if labels := obj.GetLabels(); labels[metadata.InstanceGroupLabel] == g.procGroup {
		labels[metadata.InstanceGroupLabel] = baseGroup
		obj.SetLabels(labels)
	}
}

// kroInstance reports whether obj is an unstructured custom resource in the
// base kro.run group that is NOT a ResourceGraphDefinition, i.e. an instance of
// a kind that may have been generated under a per-process group.
func kroInstance(obj client.Object) (*unstructured.Unstructured, bool) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, false
	}
	gvk := u.GroupVersionKind()
	if gvk.Group == baseGroup && gvk.Kind != rgdKind && gvk.Kind != graphKind {
		return u, true
	}
	return nil, false
}

// instanceFallback runs an operation on a kro.run instance under the per-process
// group first (the common case: instances of RGD-generated CRDs), and falls
// back to the base group if that kind is not registered (the rare case:
// instances of a user-created CRD that lives at the real kro.run group).
//
// The object's GVK is intentionally left at whichever group the successful
// operation used, NOT restored to the base group. The controller operates at
// the per-process group and stamps group-derived values (e.g. ApplySet parent
// IDs, GVK labels) into the object; specs that recompute those values from the
// returned object must therefore observe the same group the controller saw.
func (g *groupIsolatingClient) instanceFallback(u *unstructured.Unstructured, do func() error) error {
	base := u.GroupVersionKind()
	proc := base
	proc.Group = g.procGroup

	u.SetGroupVersionKind(proc)
	err := do()
	if meta.IsNoMatchError(err) {
		// The kind is not registered under the per-process group; it must be an
		// instance of a user-created CRD at the real base group.
		u.SetGroupVersionKind(base)
		return do()
	}
	return err
}

// toProcKey rewrites a CRD name from its base-group form to the per-process
// form (e.g. "testresources.kro.run" -> "testresources.p2.kro.run").
func (g *groupIsolatingClient) procCRDName(name string) string {
	return rewriteCRDNameSuffix(name, baseGroup, g.procGroup)
}

// rewriteCRDNameSuffix rewrites a CRD name of the form "<plural>.<from>" to
// "<plural>.<to>". Names that do not end in ".<from>" are returned unchanged.
func rewriteCRDNameSuffix(name, from, to string) string {
	suffix := "." + from
	if strings.HasSuffix(name, suffix) {
		return strings.TrimSuffix(name, suffix) + "." + to
	}
	return name
}

// rewriteAPIVersionGroup rewrites the group portion of an apiVersion string
// ("<group>/<version>") from -> to. Core-group versions like "v1" and
// apiVersions in other groups are returned unchanged.
func rewriteAPIVersionGroup(apiVersion, from, to string) string {
	parts := strings.SplitN(apiVersion, "/", 2)
	if len(parts) == 2 && parts[0] == from {
		return to + "/" + parts[1]
	}
	return apiVersion
}

// rewriteRawTemplateGroup deep-rewrites base-group apiVersions inside a JSON
// resource template, but only for objects whose kind is a known RGD-generated
// kind. It returns the rewritten bytes and whether parsing succeeded; on parse
// failure the caller keeps the original bytes.
func (g *groupIsolatingClient) rewriteRawTemplateGroup(raw []byte) ([]byte, bool) {
	var content map[string]interface{}
	if err := json.Unmarshal(raw, &content); err != nil {
		return nil, false
	}
	g.rewriteGroupInValue(content)
	out, err := json.Marshal(content)
	if err != nil {
		return nil, false
	}
	return out, true
}

// rewriteGroupInValue walks an arbitrary decoded-JSON value and rewrites the
// group of an object's "apiVersion" from the base group to the per-process
// group, but only when that object's "kind" is a known RGD-generated kind.
// This leaves references to user-created CRDs untouched.
func (g *groupIsolatingClient) rewriteGroupInValue(v interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		if av, ok := t["apiVersion"].(string); ok {
			if kind, ok := t["kind"].(string); ok && g.isKnownKind(kind) {
				t["apiVersion"] = rewriteAPIVersionGroup(av, baseGroup, g.procGroup)
			}
		}
		for _, val := range t {
			g.rewriteGroupInValue(val)
		}
	case []interface{}:
		for i := range t {
			g.rewriteGroupInValue(t[i])
		}
	}
}

func (g *groupIsolatingClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	if _, ok := obj.(*apiextensionsv1.CustomResourceDefinition); ok {
		// CRD names embed a group. A name like "testresources.kro.run" may refer
		// to a user-created CRD (real base group) or to a CRD generated from an
		// RGD kind (per-process group). Try the base name first, then fall back
		// to the per-process name, so both resolve transparently.
		err := g.Client.Get(ctx, key, obj, opts...)
		if apierrors.IsNotFound(err) {
			if pname := g.procCRDName(key.Name); pname != key.Name {
				pkey := key
				pkey.Name = pname
				if err2 := g.Client.Get(ctx, pkey, obj, opts...); err2 == nil {
					g.fromProc(obj)
					return nil
				}
			}
		}
		return err
	}
	if u, ok := kroInstance(obj); ok {
		return g.instanceFallback(u, func() error { return g.Client.Get(ctx, key, obj, opts...) })
	}
	g.toProc(obj)
	err := g.Client.Get(ctx, key, obj, opts...)
	g.fromProc(obj)
	return err
}

// crdWriteFallback runs a CRD write against the object's current (base) name,
// and if that reports NotFound, retries against the per-process name. This lets
// specs Delete/Patch/Update a generated CRD by its base name while leaving
// user-created CRDs (which live at the base name) untouched. When retrying, the
// CRD's spec.group is also aligned to the per-process group so that the object
// stays internally consistent (a CRD's name must equal "<plural>.<group>").
func (g *groupIsolatingClient) crdWriteFallback(obj client.Object, do func() error) error {
	err := do()
	if !apierrors.IsNotFound(err) {
		return err
	}
	orig := obj.GetName()
	pname := g.procCRDName(orig)
	if pname == orig {
		return err
	}
	obj.SetName(pname)
	var origGroup string
	if crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition); ok && crd.Spec.Group == baseGroup {
		origGroup = crd.Spec.Group
		crd.Spec.Group = g.procGroup
	}
	retryErr := do()
	obj.SetName(orig)
	if crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition); ok && origGroup != "" {
		crd.Spec.Group = origGroup
	}
	return retryErr
}

func (g *groupIsolatingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if u, ok := kroInstance(obj); ok {
		return g.instanceFallback(u, func() error { return g.Client.Create(ctx, obj, opts...) })
	}
	g.toProc(obj)
	err := g.Client.Create(ctx, obj, opts...)
	g.fromProc(obj)
	return err
}

func (g *groupIsolatingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, ok := obj.(*apiextensionsv1.CustomResourceDefinition); ok {
		return g.crdWriteFallback(obj, func() error { return g.Client.Update(ctx, obj, opts...) })
	}
	if u, ok := kroInstance(obj); ok {
		return g.instanceFallback(u, func() error { return g.Client.Update(ctx, obj, opts...) })
	}
	g.toProc(obj)
	err := g.Client.Update(ctx, obj, opts...)
	g.fromProc(obj)
	return err
}

func (g *groupIsolatingClient) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.PatchOption,
) error {
	if _, ok := obj.(*apiextensionsv1.CustomResourceDefinition); ok {
		return g.crdWriteFallback(obj, func() error { return g.Client.Patch(ctx, obj, patch, opts...) })
	}
	if u, ok := kroInstance(obj); ok {
		return g.instanceFallback(u, func() error { return g.Client.Patch(ctx, obj, patch, opts...) })
	}
	g.toProc(obj)
	err := g.Client.Patch(ctx, obj, patch, opts...)
	g.fromProc(obj)
	return err
}

func (g *groupIsolatingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if _, ok := obj.(*apiextensionsv1.CustomResourceDefinition); ok {
		return g.crdWriteFallback(obj, func() error { return g.Client.Delete(ctx, obj, opts...) })
	}
	if u, ok := kroInstance(obj); ok {
		return g.instanceFallback(u, func() error { return g.Client.Delete(ctx, obj, opts...) })
	}
	g.toProc(obj)
	err := g.Client.Delete(ctx, obj, opts...)
	g.fromProc(obj)
	return err
}
