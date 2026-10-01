// Copyright 2025 The Kube Resource Orchestrator Authors
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

package executor

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

func createWidget(t *testing.T, cl client.Client, name string) *unstructured.Unstructured {
	t.Helper()
	w := &unstructured.Unstructured{}
	w.SetGroupVersionKind(widgetGVK)
	w.SetNamespace("default")
	w.SetName(name)
	w.Object["spec"] = map[string]any{"field": "val"}
	require.NoError(t, cl.Create(t.Context(), w))
	return w
}

func getWidget(t *testing.T, cl client.Client, name string) *unstructured.Unstructured {
	t.Helper()
	w := &unstructured.Unstructured{}
	w.SetGroupVersionKind(widgetGVK)
	require.NoError(t, cl.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: name}, w))
	return w
}

func widgetStatusGraph(name string, status map[string]any) *v1alpha1.Graph {
	g := generator.NewGraph("g", generator.WithNamespace("default"),
		generator.WithPatch("p", "test.kro.run/v1", "Widget", name, map[string]any{"status": status}))
	g.SetUID(types.UID(name))
	return g
}

func statusFieldOwners(t *testing.T, obj *unstructured.Unstructured, field string) []string {
	t.Helper()
	var owners []string
	for _, mf := range obj.GetManagedFields() {
		if mf.Subresource != "status" || mf.FieldsV1 == nil {
			continue
		}
		var fields map[string]any
		require.NoError(t, json.Unmarshal(mf.FieldsV1.Raw, &fields))
		_, found, err := unstructured.NestedFieldNoCopy(fields, "f:status", "f:"+field)
		require.NoError(t, err)
		if found {
			owners = append(owners, mf.Manager+"/"+string(mf.Operation))
		}
	}
	return owners
}

func TestPatch_StatusReplace_OwnershipAndContribution(t *testing.T) {
	cl := patchEnvClient(t)
	ensureWidgetCRD(t, cl)
	for _, scenario := range []string{"legacy-first-render", "legacy-co-owned", "intermediate-main"} {
		t.Run(scenario, func(t *testing.T) {
			name := "replace-" + scenario
			widget := createWidget(t, cl, name)
			conditions := []any{map[string]any{"type": "Ready", "status": "True", "reason": "Legacy"}}
			if scenario != "intermediate-main" {
				widget.Object["status"] = map[string]any{
					"phase": "Running", "message": "stale", "conditions": conditions, "state": "ACTIVE",
				}
				// Literal legacy identity, independent of the production constant.
				require.NoError(t, cl.Status().Update(t.Context(), widget, client.FieldOwner("kro")))
				for _, field := range []string{"phase", "message", "conditions", "state"} {
					require.Equal(t, []string{"kro/Update"}, statusFieldOwners(t, widget, field))
				}
			}
			whole := widgetStatusGraph(name, map[string]any{"phase": "Running", "message": "stale"})
			manager := patchFieldManager(whole.GetUID(), "p")
			contribution := []Contribution{{
				APIVersion: "test.kro.run/v1", Kind: "Widget", Namespace: "default", Name: name,
				Subresource: "status", FieldManager: manager,
			}}
			if scenario != "legacy-first-render" {
				old, err := NewSimple(cl).Apply(t.Context(), compileAndBuildEnv(t, patchEnvCfg, whole),
					watchrouter.NoopWatcher{})
				require.NoError(t, err)
				require.Equal(t, contribution, old.Contributions)
				owners := []string{manager + "/Apply"}
				if scenario == "legacy-co-owned" {
					owners = append(owners, "kro/Update")
				}
				for _, field := range []string{"phase", "message"} {
					require.ElementsMatch(t, owners, statusFieldOwners(t, getWidget(t, cl, name), field),
						"identical old-style SSA must establish the ownership precondition")
				}

				// A changed condition transfers to the controller; an unchanged
				// legacy state remains co-owned. The first-render case stays legacy-only.
				conditions = []any{map[string]any{"type": "Ready", "status": "True", "reason": "Current"}}
				controllerStatus := &unstructured.Unstructured{}
				controllerStatus.SetGroupVersionKind(widgetGVK)
				controllerStatus.SetNamespace("default")
				controllerStatus.SetName(name)
				controllerStatus.Object["status"] = map[string]any{"conditions": conditions, "state": "ACTIVE"}
				require.NoError(t, cl.Status().Patch(t.Context(), controllerStatus, client.Apply,
					client.FieldOwner("kro-instance-status"), client.ForceOwnership))
			}
			before := getWidget(t, cl, name)
			if scenario == "intermediate-main" {
				// An identical upgraded render must keep the ledger and SSA-owned
				// values: re-keying the contribution would Release these fields.
				same, err := NewSimple(cl).Apply(t.Context(),
					compileAndBuildEnv(t, patchEnvCfg, whole, compiler.WithStatusReplace("p")), watchrouter.NoopWatcher{})
				require.NoError(t, err)
				assert.Equal(t, contribution, same.Contributions)
				after := getWidget(t, cl, name)
				assert.Equal(t, before.GetResourceVersion(), after.GetResourceVersion())
				assert.Equal(t, before.Object["status"], after.Object["status"])
				assert.Equal(t, []string{manager + "/Apply"}, statusFieldOwners(t, after, "phase"))
			}

			projection := widgetStatusGraph(name, map[string]any{"phase": "Running"})
			res, err := NewSimple(cl).Apply(t.Context(),
				compileAndBuildEnv(t, patchEnvCfg, projection, compiler.WithStatusReplace("p")), watchrouter.NoopWatcher{})
			require.NoError(t, err)
			assert.Equal(t, contribution, res.Contributions)
			assert.Empty(t, res.Applied)
			after := getWidget(t, cl, name)
			assert.Equal(t, map[string]any{
				"phase": "Running", "conditions": conditions, "state": "ACTIVE",
			}, after.Object["status"], "omitted author fields disappear despite other owners")
			assert.Empty(t, statusFieldOwners(t, after, "message"), "removal drops the field from every owner")
			for _, field := range []string{"phase", "conditions", "state"} {
				assert.ElementsMatch(t, statusFieldOwners(t, before, field), statusFieldOwners(t, after, field),
					"unchanged %s keeps its owners", field)
			}

			// A re-added field is written under the literal legacy Update identity.
			restored := widgetStatusGraph(name, map[string]any{"phase": "Running", "message": "restored"})
			res, err = NewSimple(cl).Apply(t.Context(),
				compileAndBuildEnv(t, patchEnvCfg, restored, compiler.WithStatusReplace("p")), watchrouter.NoopWatcher{})
			require.NoError(t, err)
			assert.Equal(t, contribution, res.Contributions)
			after = getWidget(t, cl, name)
			message, _, _ := unstructured.NestedString(after.Object, "status", "message")
			assert.Equal(t, "restored", message)
			assert.Equal(t, []string{"kro/Update"}, statusFieldOwners(t, after, "message"))
		})
	}
}
