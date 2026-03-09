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

package simpleschema

import (
	"fmt"
	"strings"
)

// PrinterColumn describes a field marked with kubectlPrint in SimpleSchema.
// It captures only the logical path and the inferred schema type.
type PrinterColumn struct {
	Path       []string
	TargetType string
	Title      string
}

type printerColumnConfig struct {
	Enabled bool
	Title   string
}

func parsePrinterColumnConfig(markers []*Marker) (printerColumnConfig, error) {
	config := printerColumnConfig{}
	for _, marker := range markers {
		if marker.MarkerType != MarkerTypeKubectlPrint {
			continue
		}

		if config.Enabled {
			return printerColumnConfig{}, fmt.Errorf("duplicate kubectlPrint marker; only one is allowed per field")
		}

		config.Enabled = true
		config.Title = strings.TrimSpace(marker.Value)
		if config.Title == "" {
			return printerColumnConfig{}, fmt.Errorf("kubectlPrint requires a non-empty title")
		}
	}
	return config, nil
}
