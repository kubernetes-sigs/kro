// Copyright 2026 The Kubernetes Authors.
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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEnvironmentStop_NilReceiver(t *testing.T) {
	t.Parallel()

	var env *Environment
	require.NoError(t, env.Stop())
}

func TestEnvironmentStop_DoesNotBlockAfterManagerExitObserved(t *testing.T) {
	t.Parallel()

	env := &Environment{
		managerReady: make(chan struct{}),
		managerDone:  make(chan struct{}),
	}
	env.managerErr = errors.New("manager boom")
	close(env.managerDone)

	err := env.waitForManagerReady()
	require.EqualError(t, err, "manager exited before becoming ready: manager boom")

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- env.Stop()
	}()

	select {
	case stopErr := <-stopDone:
		require.EqualError(t, stopErr, "manager boom")
	case <-time.After(time.Second):
		t.Fatal("Stop blocked after manager exit was already observed")
	}
}

func TestEnvironmentStop_IgnoresContextCanceledManagerExit(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	env := &Environment{
		context:     ctx,
		cancel:      cancel,
		managerDone: make(chan struct{}),
	}
	env.managerErr = context.Canceled
	close(env.managerDone)

	assert.NoError(t, env.Stop())
}

func TestGroupIsolatingClient_CRDUpdate(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		baseExists bool
		conflict   string
	}{
		{name: "process CRD with missing base"},
		{name: "base CRD takes precedence", baseExists: true},
		{name: "process CRD UID conflict", conflict: "UID"},
		{name: "process CRD resourceVersion conflict", conflict: "resourceVersion"},
		{name: "base CRD UID conflict", baseExists: true, conflict: "UID"},
		{name: "base CRD resourceVersion conflict", baseExists: true, conflict: "resourceVersion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			processCRD := isolationTestCRD("p2.kro.run")
			objects := []*apiextensionsv1.CustomResourceDefinition{processCRD}
			target := processCRD
			if tc.baseExists {
				target = isolationTestCRD(baseGroup)
				objects = append(objects, target)
			}
			rawClient, writes := newCRDUpdateTestClient(t, objects...)
			c := NewGroupIsolatingClient(rawClient, "p2")

			crd := &apiextensionsv1.CustomResourceDefinition{}
			key := client.ObjectKey{Name: "testwatches.kro.run"}
			require.NoError(t, c.Get(t.Context(), key, crd))
			assert.Equal(t, target.UID, crd.UID)
			crd.Labels = map[string]string{"updated": "true"}
			switch tc.conflict {
			case "UID":
				crd.UID = "stale-uid"
			case "resourceVersion":
				crd.ResourceVersion = "9"
			}
			uid, resourceVersion := crd.UID, crd.ResourceVersion

			err := c.Update(t.Context(), crd)
			if tc.conflict == "" {
				require.NoError(t, err)
				assert.Equal(t, "2", crd.ResourceVersion)
			} else {
				require.Error(t, err)
				assert.True(t, apierrors.IsConflict(err), "%v", err)
				assert.Contains(t, err.Error(), tc.conflict)
				assert.Equal(t, resourceVersion, crd.ResourceVersion)
			}

			require.Len(t, writes, 1)
			written := <-writes
			assert.Equal(t, target.Name, written.Name)
			assert.Equal(t, target.Spec.Group, written.Spec.Group)
			assert.Equal(t, uid, written.UID)
			assert.Equal(t, resourceVersion, written.ResourceVersion)
			assert.Equal(t, "true", written.Labels["updated"])
			assert.Equal(t, key.Name, crd.Name)
			assert.Equal(t, baseGroup, crd.Spec.Group)
			assert.Equal(t, uid, crd.UID)
		})
	}
}

func TestGroupIsolatingClient_CRDPatchAndDeleteRouting(t *testing.T) {
	t.Parallel()

	for _, baseExists := range []bool{false, true} {
		t.Run(fmt.Sprintf("baseExists=%t", baseExists), func(t *testing.T) {
			t.Parallel()

			scheme := runtime.NewScheme()
			require.NoError(t, apiextensionsv1.AddToScheme(scheme))
			processCRD := isolationTestCRD("p2.kro.run")
			objects := []client.Object{processCRD}
			target := processCRD
			if baseExists {
				target = isolationTestCRD(baseGroup)
				objects = append(objects, target)
			}
			rawClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			c := NewGroupIsolatingClient(rawClient, "p2")
			key := client.ObjectKey{Name: "testwatches.kro.run"}
			crd := &apiextensionsv1.CustomResourceDefinition{}
			require.NoError(t, c.Get(t.Context(), key, crd))

			before := crd.DeepCopy()
			crd.Labels = map[string]string{"updated": "true"}
			require.NoError(t, c.Patch(t.Context(), crd, client.MergeFrom(before)))
			actual := &apiextensionsv1.CustomResourceDefinition{}
			targetKey := client.ObjectKeyFromObject(target)
			require.NoError(t, rawClient.Get(t.Context(), targetKey, actual))
			assert.Equal(t, target.Spec.Group, actual.Spec.Group)
			assert.Equal(t, "true", actual.Labels["updated"])
			assert.Equal(t, key.Name, crd.Name)
			assert.Equal(t, baseGroup, crd.Spec.Group)

			require.NoError(t, c.Delete(t.Context(), crd))
			assert.True(t, apierrors.IsNotFound(rawClient.Get(t.Context(), targetKey, actual)))
			assert.Equal(t, key.Name, crd.Name)
			assert.Equal(t, baseGroup, crd.Spec.Group)
			if baseExists {
				require.NoError(t, rawClient.Get(t.Context(), client.ObjectKeyFromObject(processCRD), actual))
				assert.Empty(t, actual.Labels)
			}
		})
	}
}

func isolationTestCRD(group string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiextensionsv1.SchemeGroupVersion.String(),
			Kind:       "CustomResourceDefinition",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            "testwatches." + group,
			UID:             types.UID("uid-" + group),
			ResourceVersion: "1",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:   "TestWatch",
				Plural: "testwatches",
			},
		},
	}
}

func newCRDUpdateTestClient(t *testing.T, objects ...*apiextensionsv1.CustomResourceDefinition) (client.Client, <-chan *apiextensionsv1.CustomResourceDefinition) {
	t.Helper()
	stored := make(map[string]*apiextensionsv1.CustomResourceDefinition, len(objects))
	for _, obj := range objects {
		stored[obj.Name] = obj
	}
	writes := make(chan *apiextensionsv1.CustomResourceDefinition, 4)
	resource := apiextensionsv1.SchemeGroupVersion.WithResource("customresourcedefinitions").GroupResource()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON := func(code int, obj any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			assert.NoError(t, json.NewEncoder(w).Encode(obj))
		}
		writeError := func(err *apierrors.StatusError) {
			status := err.Status()
			status.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}
			writeJSON(int(status.Code), status)
		}
		name, ok := strings.CutPrefix(r.URL.Path, "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/")
		if !assert.True(t, ok, "unexpected request: %s %s", r.Method, r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		current := stored[name]
		switch r.Method {
		case http.MethodGet:
			if current == nil {
				writeError(apierrors.NewNotFound(resource, name))
				return
			}
			writeJSON(http.StatusOK, current)
		case http.MethodPut:
			updated := &apiextensionsv1.CustomResourceDefinition{}
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(updated)) {
				http.Error(w, "invalid CRD", http.StatusBadRequest)
				return
			}
			assert.Equal(t, name, updated.Name)
			writes <- updated.DeepCopy()
			var currentUID types.UID
			if current != nil {
				currentUID = current.UID
			}
			// Kubernetes 1.31 checks the UID before reporting a missing update target.
			if updated.UID != "" && updated.UID != currentUID {
				writeError(apierrors.NewConflict(resource, name, fmt.Errorf("UID precondition failed: %q != %q", updated.UID, currentUID)))
				return
			}
			if current == nil {
				writeError(apierrors.NewNotFound(resource, name))
				return
			}
			if updated.ResourceVersion != current.ResourceVersion {
				writeError(apierrors.NewConflict(resource, name, fmt.Errorf("resourceVersion conflict: %q != %q", updated.ResourceVersion, current.ResourceVersion)))
				return
			}
			updated.ResourceVersion = "2"
			writeJSON(http.StatusOK, updated)
		default:
			t.Errorf("unexpected request method: %s", r.Method)
			http.Error(w, "unsupported method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)

	scheme := runtime.NewScheme()
	require.NoError(t, apiextensionsv1.AddToScheme(scheme))
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{apiextensionsv1.SchemeGroupVersion})
	mapper.Add(apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinition"), meta.RESTScopeRoot)
	c, err := client.New(&rest.Config{
		Host:          server.URL,
		ContentConfig: rest.ContentConfig{ContentType: runtime.ContentTypeJSON},
	}, client.Options{Scheme: scheme, Mapper: mapper})
	require.NoError(t, err)
	return c, writes
}
