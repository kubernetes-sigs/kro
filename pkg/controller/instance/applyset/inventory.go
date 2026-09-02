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

package applyset

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
)

type parentObject interface {
	metav1.Object
	schema.ObjectKind
}

// ValidateParentInventory checks the persisted ApplySet metadata that is the
// authoritative resource-discovery scope during deletion. An empty inventory
// is valid, but its annotation keys must still be present.
func ValidateParentInventory(parent parentObject) error {
	expectedID := ID(parent)
	if actual := parent.GetLabels()[ApplySetParentIDLabel]; actual != expectedID {
		return fmt.Errorf("invalid %s label: got %q, want %q", ApplySetParentIDLabel, actual, expectedID)
	}

	annotations := parent.GetAnnotations()
	tooling, exists := annotations[ApplySetToolingAnnotation]
	if !exists || !strings.HasPrefix(tooling, "kro/") {
		return fmt.Errorf("invalid %s annotation: %q is not owned by kro", ApplySetToolingAnnotation, tooling)
	}

	if _, exists := annotations[ApplySetGKsAnnotation]; !exists {
		return fmt.Errorf("missing required %s annotation", ApplySetGKsAnnotation)
	}
	if _, exists := annotations[ApplySetAdditionalNamespacesAnnotation]; !exists {
		return fmt.Errorf("missing required %s annotation", ApplySetAdditionalNamespacesAnnotation)
	}

	groupKinds, namespaces, err := parseParentAnnotationSets(annotations)
	if err != nil {
		return err
	}

	// The hash is optional for ApplySet parents created by earlier kro
	// versions. Normal reconciliation backfills it before applying children.
	if actual, exists := annotations[ApplySetInventoryHashAnnotation]; exists {
		expected := inventoryHash(expectedID, groupKinds, namespaces)
		if actual != expected {
			return fmt.Errorf("invalid %s annotation: got %q, want %q", ApplySetInventoryHashAnnotation, actual, expected)
		}
	}
	return nil
}

// parseParentAnnotationSets is the single parser for the persisted ApplySet
// discovery scope used by both normal projection and deletion validation.
func parseParentAnnotationSets(
	annotations map[string]string,
) (sets.Set[schema.GroupKind], sets.Set[string], error) {
	groupKinds, err := parseGroupKinds(annotations[ApplySetGKsAnnotation])
	if err != nil {
		return nil, nil, fmt.Errorf("invalid %s annotation: %w", ApplySetGKsAnnotation, err)
	}
	namespaces, err := parseNamespaces(annotations[ApplySetAdditionalNamespacesAnnotation])
	if err != nil {
		return nil, nil, fmt.Errorf("invalid %s annotation: %w", ApplySetAdditionalNamespacesAnnotation, err)
	}
	return groupKinds, namespaces, nil
}

func parseGroupKinds(raw string) (sets.Set[schema.GroupKind], error) {
	result := sets.New[schema.GroupKind]()
	if raw == "" {
		return result, nil
	}
	for entry := range strings.SplitSeq(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, fmt.Errorf("contains an empty group-kind")
		}
		parts := strings.SplitN(entry, ".", 2)
		if parts[0] == "" || strings.ContainsAny(parts[0], " \t\r\n") {
			return nil, fmt.Errorf("invalid kind %q", parts[0])
		}
		gk := schema.GroupKind{Kind: parts[0]}
		if len(parts) == 2 {
			if problems := validation.IsDNS1123Subdomain(parts[1]); len(problems) > 0 {
				return nil, fmt.Errorf("invalid group %q: %s", parts[1], strings.Join(problems, ", "))
			}
			gk.Group = parts[1]
		}
		result.Insert(gk)
	}
	return result, nil
}

func parseNamespaces(raw string) (sets.Set[string], error) {
	result := sets.New[string]()
	if raw == "" {
		return result, nil
	}
	for entry := range strings.SplitSeq(raw, ",") {
		entry = strings.TrimSpace(entry)
		if problems := validation.IsDNS1123Label(entry); len(problems) > 0 {
			return nil, fmt.Errorf("invalid namespace %q: %s", entry, strings.Join(problems, ", "))
		}
		result.Insert(entry)
	}
	return result, nil
}

func inventoryHash(
	id string,
	groupKinds sets.Set[schema.GroupKind],
	namespaces sets.Set[string],
) string {
	gkStrings := make([]string, 0, groupKinds.Len())
	for gk := range groupKinds {
		value := gk.Kind
		if gk.Group != "" {
			value += "." + gk.Group
		}
		gkStrings = append(gkStrings, value)
	}
	slices.Sort(gkStrings)
	nsStrings := namespaces.UnsortedList()
	slices.Sort(nsStrings)
	payload := strings.Join([]string{id, strings.Join(gkStrings, ","), strings.Join(nsStrings, ",")}, "\x00")
	sum := sha256.Sum256([]byte(payload))
	return "sha256:" + base64.RawURLEncoding.EncodeToString(sum[:])
}
