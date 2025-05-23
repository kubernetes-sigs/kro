package client

import (
	"testing"

	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kro-run/kro/pkg/metadata"
	"github.com/stretchr/testify/assert"
)

func TestCRDWrapper_verifyNoConflict(t *testing.T) {
	tests := []struct {
		name        string
		existingCRD *v1.CustomResourceDefinition
		newCRD      v1.CustomResourceDefinition
		wantErr     bool
		errMsg      string
	}{
		{
			name: "successful verification - same owner",
			existingCRD: &v1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test.crd",
					Labels: map[string]string{
						metadata.OwnedLabel:                       "true",
						metadata.ResourceGraphDefinitionNameLabel: "test-rgd",
						metadata.ResourceGraphDefinitionIDLabel:   "test-id",
					},
				},
			},
			newCRD: v1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test.crd",
					Labels: map[string]string{
						metadata.OwnedLabel:                       "true",
						metadata.ResourceGraphDefinitionNameLabel: "test-rgd",
						metadata.ResourceGraphDefinitionIDLabel:   "test-id",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "conflict - not owned by KRO",
			existingCRD: &v1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test.crd",
					Labels: map[string]string{
						metadata.ResourceGraphDefinitionNameLabel: "test-rgd",
						metadata.ResourceGraphDefinitionIDLabel:   "test-id",
					},
				},
			},
			newCRD: v1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test.crd",
					Labels: map[string]string{
						metadata.OwnedLabel:                       "true",
						metadata.ResourceGraphDefinitionNameLabel: "test-rgd",
						metadata.ResourceGraphDefinitionIDLabel:   "test-id",
					},
				},
			},
			wantErr: true,
			errMsg:  "conflict detected: CRD test.crd already exists and is not owned by KRO",
		},
		{
			name: "conflict - different RGD name",
			existingCRD: &v1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test.crd",
					Labels: map[string]string{
						metadata.OwnedLabel:                       "true",
						metadata.ResourceGraphDefinitionNameLabel: "existing-rgd",
						metadata.ResourceGraphDefinitionIDLabel:   "test-id",
					},
				},
			},
			newCRD: v1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test.crd",
					Labels: map[string]string{
						metadata.OwnedLabel:                       "true",
						metadata.ResourceGraphDefinitionNameLabel: "new-rgd",
						metadata.ResourceGraphDefinitionIDLabel:   "test-id",
					},
				},
			},
			wantErr: true,
			errMsg:  "conflict detected: CRD test.crd has ownership by another ResourceGraphDefinition",
		},
		{
			name: "conflict - different RGD ID",
			existingCRD: &v1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test.crd",
					Labels: map[string]string{
						metadata.OwnedLabel:                       "true",
						metadata.ResourceGraphDefinitionNameLabel: "test-rgd",
						metadata.ResourceGraphDefinitionIDLabel:   "existing-id",
					},
				},
			},
			newCRD: v1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test.crd",
					Labels: map[string]string{
						metadata.OwnedLabel:                       "true",
						metadata.ResourceGraphDefinitionNameLabel: "test-rgd",
						metadata.ResourceGraphDefinitionIDLabel:   "new-id",
					},
				},
			},
			wantErr: true,
			errMsg:  "conflict detected: CRD test.crd has ownership by another ResourceGraphDefinition",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &CRDWrapper{}
			err := w.verifyNoConflict(tt.existingCRD, tt.newCRD)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, tt.errMsg, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
