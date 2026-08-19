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
	"encoding/json"
	"fmt"

	"k8s.io/client-go/rest"
)

// connInfo is the minimal, serializable subset of a rest.Config needed for a
// secondary process to connect to a control plane started by another process.
//
// It is used to hand the shared apiserver connection details from Ginkgo's
// SynchronizedBeforeSuite process #1 (which owns the envtest control plane) to
// every other parallel process over the []byte channel Ginkgo provides.
type connInfo struct {
	Host        string  `json:"host"`
	CAData      []byte  `json:"caData,omitempty"`
	CertData    []byte  `json:"certData,omitempty"`
	KeyData     []byte  `json:"keyData,omitempty"`
	BearerToken string  `json:"bearerToken,omitempty"`
	QPS         float32 `json:"qps,omitempty"`
	Burst       int     `json:"burst,omitempty"`
}

// EncodeRESTConfig serializes the connection-relevant fields of a rest.Config
// so they can be broadcast to other Ginkgo parallel processes.
func EncodeRESTConfig(cfg *rest.Config) ([]byte, error) {
	if cfg == nil {
		return nil, fmt.Errorf("nil rest config")
	}
	ci := connInfo{
		Host:        cfg.Host,
		CAData:      cfg.TLSClientConfig.CAData,
		CertData:    cfg.TLSClientConfig.CertData,
		KeyData:     cfg.TLSClientConfig.KeyData,
		BearerToken: cfg.BearerToken,
		QPS:         cfg.QPS,
		Burst:       cfg.Burst,
	}
	return json.Marshal(ci)
}

// DecodeRESTConfig reconstructs a usable rest.Config from bytes produced by
// EncodeRESTConfig.
func DecodeRESTConfig(data []byte) (*rest.Config, error) {
	var ci connInfo
	if err := json.Unmarshal(data, &ci); err != nil {
		return nil, fmt.Errorf("decoding shared rest config: %w", err)
	}
	return &rest.Config{
		Host:        ci.Host,
		BearerToken: ci.BearerToken,
		QPS:         ci.QPS,
		Burst:       ci.Burst,
		TLSClientConfig: rest.TLSClientConfig{
			CAData:   ci.CAData,
			CertData: ci.CertData,
			KeyData:  ci.KeyData,
		},
	}, nil
}
