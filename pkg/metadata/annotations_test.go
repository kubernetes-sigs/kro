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

package metadata

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)



func TestGetResourceGraphDefinitionName(t *testing.T) {
	longName := strings.Repeat("a", 100)

	cases := []struct {
		name         string
		labels       map[string]string
		annotations  map[string]string
		expectedName string
		expectedOK   bool
	}{
		{
			name:       "neither annotation nor label",
			expectedOK: false,
		},
		{
			name:         "label only (pre-annotation object)",
			labels:       map[string]string{ResourceGraphDefinitionNameLabel: "my-rgd"},
			expectedName: "my-rgd",
			expectedOK:   true,
		},
		{
			name:         "annotation only (over-long name)",
			annotations:  map[string]string{ResourceGraphDefinitionNameAnnotation: longName},
			expectedName: longName,
			expectedOK:   true,
		},
		{
			name:         "annotation preferred over label",
			labels:       map[string]string{ResourceGraphDefinitionNameLabel: "stale"},
			annotations:  map[string]string{ResourceGraphDefinitionNameAnnotation: "my-rgd"},
			expectedName: "my-rgd",
			expectedOK:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := &mockObject{ObjectMeta: metav1.ObjectMeta{
				Labels:      tc.labels,
				Annotations: tc.annotations,
			}}
			name, ok := GetResourceGraphDefinitionName(obj)
			assert.Equal(t, tc.expectedOK, ok)
			assert.Equal(t, tc.expectedName, name)
		})
	}
}
