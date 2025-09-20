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

package fake

import (
	"context"

	"github.com/kro-run/kro/pkg/client"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	authclient "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/rest"
)

// FakeSet is a fake implementation of SetInterface for testing
type FakeSet struct {
	DynamicClient       dynamic.Interface
	KubernetesClient    kubernetes.Interface
	ApiExtensionsClient apiextensionsv1.ApiextensionsV1Interface
	AuthorizationClient authclient.AuthorizationV1Interface
	Config              *rest.Config
}

var _ client.SetInterface = (*FakeSet)(nil)

// NewFakeSet creates a new FakeSet with the given clients
func NewFakeSet(dynamicClient dynamic.Interface) *FakeSet {
	return &FakeSet{
		DynamicClient: NewJSONSafeDynamicClient(dynamicClient),
		Config:        &rest.Config{},
	}
}

// Kubernetes returns the standard Kubernetes clientset
func (f *FakeSet) Kubernetes() kubernetes.Interface {
	return f.KubernetesClient
}

// Dynamic returns the dynamic client
func (f *FakeSet) Dynamic() dynamic.Interface {
	return f.DynamicClient
}

func (f *FakeSet) Authorization() authclient.AuthorizationV1Interface {
	return f.AuthorizationClient
}

// APIExtensionsV1 returns the API extensions client
func (f *FakeSet) APIExtensionsV1() apiextensionsv1.ApiextensionsV1Interface {
	return f.ApiExtensionsClient
}

// RESTConfig returns a copy of the underlying REST config
func (f *FakeSet) RESTConfig() *rest.Config {
	return f.Config
}

// CRD returns a new CRD interface instance
func (f *FakeSet) CRD(cfg client.CRDWrapperConfig) client.CRDInterface {
	// Return a fake CRD implementation for testing
	return &FakeCRD{}
}

// WithImpersonation returns a new client that impersonates the given user
// For testing, this just returns the same fake client
func (f *FakeSet) WithImpersonation(user string) (client.SetInterface, error) {
	return f, nil
}

// FakeCRD is a fake implementation of CRDInterface for testing
type FakeCRD struct{}

var _ client.CRDInterface = (*FakeCRD)(nil)

// Ensure ensures a CRD exists, up-to-date, and is ready
func (f *FakeCRD) Ensure(ctx context.Context, crd v1.CustomResourceDefinition) error {
	// For testing, just return success
	return nil
}

// Get retrieves a CRD by name
func (f *FakeCRD) Get(ctx context.Context, name string) (*v1.CustomResourceDefinition, error) {
	// For testing, return an empty CRD
	return &v1.CustomResourceDefinition{}, nil
}

// Delete removes a CRD if it exists
func (f *FakeCRD) Delete(ctx context.Context, name string) error {
	// For testing, just return success
	return nil
}
