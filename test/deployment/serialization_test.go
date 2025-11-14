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

package deployment_test

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"

	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/scheme"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
)

type ResourceRef struct {
	APIGroup   string
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
}

func (r *ResourceRef) String() string {
	return fmt.Sprintf("%s/%s kind:%s object: %s/%s", r.APIGroup, r.APIVersion, r.Kind, r.Namespace, r.Name)
}

type ParseError struct {
	Data []byte
	Err  error
}

func (p *ParseError) Error() string {
	return fmt.Sprintf("error parsing data %s: %v", string(p.Data), p.Err.Error())
}

func commentOnly(d []byte) bool {
	for _, b := range bytes.Split(d, []byte("\n")) {
		line := strings.TrimPrefix(string(b), " ")
		if line != "" && !strings.HasPrefix(line, "#") {
			return false
		}
	}
	return true
}

func ParseUnstructured(r io.Reader) (map[ResourceRef]*unstructured.Unstructured, error) {
	objects := map[ResourceRef]*unstructured.Unstructured{}
	kubereader := kubeyaml.NewYAMLReader(bufio.NewReader(r))
	for {
		data, err := kubereader.Read()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		data = bytes.TrimLeft(data, "---")
		if !commentOnly(data) {
			o := &unstructured.Unstructured{}
			_, _, err := scheme.Codecs.UniversalDeserializer().Decode(data, nil, o)
			if err != nil {
				return nil, &ParseError{
					Data: data,
					Err:  err,
				}
			}
			objectRef := ResourceRef{
				APIGroup:   o.GetAPIVersion(),
				APIVersion: o.GetAPIVersion(),
				Kind:       o.GetKind(),
				Namespace:  o.GetNamespace(),
				Name:       o.GetName(),
			}

			if _, ok := objects[objectRef]; ok {
				return nil, &ParseError{
					Data: data,
					Err:  fmt.Errorf("duplicate object found: %s", objectRef.String()),
				}
			}

			objects[objectRef] = o
		}
	}
	return objects, nil
}
