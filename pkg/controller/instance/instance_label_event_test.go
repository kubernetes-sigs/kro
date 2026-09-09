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

package instance

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"

	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

func instanceWith(name, group string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetName(name)
	obj.SetNamespace("default")
	obj.SetUID("instance-uid")
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: "v1", Kind: "MyApp"})
	return obj
}

func drain(recorder *record.FakeRecorder) []string {
	var got []string
	for len(recorder.Events) > 0 {
		got = append(got, <-recorder.Events)
	}
	return got
}

func TestWarnOnEncodedInstanceLabels(t *testing.T) {
	t.Parallel()

	longName := "instance-" + strings.Repeat("x", 60)
	inst := instanceWith(longName, "apps.example.com")

	recorder := record.NewFakeRecorder(10)
	c := &Controller{eventRecorder: recorder}
	c.warnOnEncodedInstanceLabels(inst, metadata.InstanceIdentityAnnotations(inst))

	got := drain(recorder)
	require.Len(t, got, 1, "only the over-long name should warn")
	assert.Contains(t, got[0], "Warning InstanceLabelEncoded")
	assert.Contains(t, got[0], longName)
	assert.Contains(t, got[0], metadata.LabelValueToken(longName))
	assert.Contains(t, got[0], metadata.InstanceLabel)
	assert.Contains(t, got[0], metadata.InstanceNameAnnotation)
}

func TestWarnOnEncodedInstanceLabels_NameAndGroup(t *testing.T) {
	t.Parallel()

	inst := instanceWith("instance-"+strings.Repeat("x", 60), strings.Repeat("sub.", 20)+"example.com")

	recorder := record.NewFakeRecorder(10)
	c := &Controller{eventRecorder: recorder}
	c.warnOnEncodedInstanceLabels(inst, metadata.InstanceIdentityAnnotations(inst))

	assert.Len(t, drain(recorder), 2)
}

func TestWarnOnEncodedInstanceLabels_Quiet(t *testing.T) {
	t.Parallel()

	inst := instanceWith("instance-name", "apps.example.com")

	recorder := record.NewFakeRecorder(10)
	c := &Controller{eventRecorder: recorder}
	c.warnOnEncodedInstanceLabels(inst, metadata.InstanceIdentityAnnotations(inst))
	assert.Empty(t, drain(recorder))

	noRecorder := &Controller{}
	assert.NotPanics(t, func() {
		long := instanceWith(strings.Repeat("x", 100), "apps.example.com")
		noRecorder.warnOnEncodedInstanceLabels(long, metadata.InstanceIdentityAnnotations(long))
	})
}

func TestApplyInstanceIdentityAnnotations(t *testing.T) {
	t.Parallel()

	obj := &unstructured.Unstructured{}
	obj.SetAnnotations(map[string]string{"keep": "me"})
	applyInstanceIdentityAnnotations(obj, map[string]string{metadata.InstanceNameAnnotation: "full-name"})

	assert.Equal(t, map[string]string{
		"keep":                          "me",
		metadata.InstanceNameAnnotation: "full-name",
	}, obj.GetAnnotations())

	untouched := &unstructured.Unstructured{}
	applyInstanceIdentityAnnotations(untouched, nil)
	assert.Nil(t, untouched.GetAnnotations())
}
