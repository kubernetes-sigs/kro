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

package compat

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Report predicates decide whether an RGD update is allowed through, and
// the Description strings are the only explanation an author gets for a
// rejection. Both were uncovered: a wrong predicate silently permits a breaking
// update or blocks a safe one, and a wrong description leaves the author unable
// to tell which field is at fault.

func TestReportPredicates(t *testing.T) {
	t.Parallel()

	empty := &Report{}
	assert.True(t, empty.IsCompatible())
	assert.False(t, empty.HasBreakingChanges())
	assert.False(t, empty.HasChanges())

	nonBreaking := &Report{}
	nonBreaking.AddNonBreakingChange("spec.replicas", PropertyAdded, "", "optional")
	assert.True(t, nonBreaking.IsCompatible(),
		"a non-breaking change must not block the update")
	assert.False(t, nonBreaking.HasBreakingChanges())
	assert.True(t, nonBreaking.HasChanges(),
		"HasChanges must see non-breaking changes too, or a no-op update is indistinguishable")

	breaking := &Report{}
	breaking.AddBreakingChange("spec.name", PropertyRemoved, "string", "")
	assert.False(t, breaking.IsCompatible())
	assert.True(t, breaking.HasBreakingChanges())
	assert.True(t, breaking.HasChanges())
}

func TestReportString(t *testing.T) {
	t.Parallel()

	t.Run("a compatible report says so explicitly", func(t *testing.T) {
		t.Parallel()
		r := &Report{}
		r.AddNonBreakingChange("spec.a", PropertyAdded, "", "optional")
		assert.Equal(t, "no breaking changes", r.String(),
			"non-breaking changes must not be reported as breaking")
	})

	t.Run("changes are joined in order", func(t *testing.T) {
		t.Parallel()
		r := &Report{}
		r.AddBreakingChange("spec.a", PropertyRemoved, "", "")
		r.AddBreakingChange("spec.b", PropertyRemoved, "", "")
		out := r.String()
		assert.Equal(t, "Property a was removed; Property b was removed", out)
	})

	t.Run("exactly the summary limit is not truncated", func(t *testing.T) {
		t.Parallel()
		r := &Report{}
		for _, p := range []string{"spec.a", "spec.b", "spec.c"} {
			r.AddBreakingChange(p, PropertyRemoved, "", "")
		}
		out := r.String()
		assert.NotContains(t, out, "more changes",
			"a report at the limit must list every change")
		assert.Equal(t, 3, strings.Count(out, "was removed"))
	})

	t.Run("beyond the limit the remainder is counted", func(t *testing.T) {
		t.Parallel()
		r := &Report{}
		for _, p := range []string{"spec.a", "spec.b", "spec.c", "spec.d", "spec.e"} {
			r.AddBreakingChange(p, PropertyRemoved, "", "")
		}
		out := r.String()
		assert.Contains(t, out, "Property a was removed")
		assert.Contains(t, out, "and 2 more changes",
			"the count must cover every change past the limit")
		assert.NotContains(t, out, "Property d",
			"changes past the limit are summarised, not listed")
	})
}

// lastPathComponent is what turns a JSON path into the property name an author
// recognises, so it must never return an empty description for a real path.
func TestLastPathComponent(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "replicas", lastPathComponent("spec.template.replicas"))
	assert.Equal(t, "replicas", lastPathComponent("replicas"))
	assert.Equal(t, "", lastPathComponent(""))
}

// Every ChangeType must produce a description that names what changed. A type
// that fell through to the unknown-change fallback would tell the author
// nothing useful, so each is asserted individually.
func TestChangeDescriptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		change   Change
		contains []string
	}{
		{"property removed", Change{Path: "spec.name", ChangeType: PropertyRemoved},
			[]string{"Property", "name", "removed"}},
		{"required property added", Change{Path: "spec.name", ChangeType: PropertyAdded, NewValue: "required"},
			[]string{"Required property", "name", "added"}},
		{"optional property added", Change{Path: "spec.name", ChangeType: PropertyAdded, NewValue: "optional"},
			[]string{"Optional property", "name", "added"}},
		{"type changed", Change{Path: "spec.n", ChangeType: TypeChanged, OldValue: "string", NewValue: "integer"},
			[]string{"Type changed", "string", "integer"}},
		{"required added", Change{Path: "spec", ChangeType: RequiredAdded, NewValue: "name"},
			[]string{"name", "newly required"}},
		{"required removed", Change{Path: "spec", ChangeType: RequiredRemoved, OldValue: "name"},
			[]string{"name", "no longer required"}},
		{"enum restricted", Change{Path: "spec.e", ChangeType: EnumRestricted, OldValue: "blue"},
			[]string{"Enum value", "blue", "removed"}},
		{"enum expanded", Change{Path: "spec.e", ChangeType: EnumExpanded, NewValue: "green"},
			[]string{"Enum value", "green", "added"}},
		{"pattern changed", Change{Path: "spec.p", ChangeType: PatternChanged, OldValue: "^a", NewValue: "^b"},
			[]string{"pattern changed", "^a", "^b"}},
		{"pattern added", Change{Path: "spec.p", ChangeType: PatternAdded, NewValue: "^b"},
			[]string{"pattern", "^b", "added"}},
		{"pattern removed", Change{Path: "spec.p", ChangeType: PatternRemoved, OldValue: "^a"},
			[]string{"pattern", "^a", "removed"}},
		{"required default removed", Change{Path: "spec.d", ChangeType: RequiredDefaultRemoved, OldValue: "d"},
			[]string{"Default value removed", "required field"}},
		{"description changed", Change{Path: "spec.d", ChangeType: DescriptionChanged, OldValue: "a", NewValue: "b"},
			[]string{"Description", "changed"}},
		{"default changed", Change{Path: "spec.d", ChangeType: DefaultChanged, OldValue: "1", NewValue: "2"},
			[]string{"Default value was changed", "1", "2"}},

		{"minimum added", Change{ChangeType: MinimumAdded, NewValue: "1"}, []string{"Minimum constraint", "added"}},
		{"minimum removed", Change{ChangeType: MinimumRemoved, OldValue: "1"}, []string{"Minimum constraint", "removed"}},
		{"minimum increased", Change{ChangeType: MinimumIncreased, OldValue: "1", NewValue: "2"}, []string{"Minimum was increased"}},
		{"minimum decreased", Change{ChangeType: MinimumDecreased, OldValue: "2", NewValue: "1"}, []string{"Minimum was decreased"}},
		{"maximum added", Change{ChangeType: MaximumAdded, NewValue: "9"}, []string{"Maximum constraint", "added"}},
		{"maximum removed", Change{ChangeType: MaximumRemoved, OldValue: "9"}, []string{"Maximum constraint", "removed"}},
		{"maximum increased", Change{ChangeType: MaximumIncreased, OldValue: "8", NewValue: "9"}, []string{"Maximum was increased"}},
		{"maximum decreased", Change{ChangeType: MaximumDecreased, OldValue: "9", NewValue: "8"}, []string{"Maximum was decreased"}},

		{"minlength added", Change{ChangeType: MinLengthAdded, NewValue: "1"}, []string{"MinLength constraint", "added"}},
		{"minlength removed", Change{ChangeType: MinLengthRemoved, OldValue: "1"}, []string{"MinLength constraint", "removed"}},
		{"minlength increased", Change{ChangeType: MinLengthIncreased, OldValue: "1", NewValue: "2"}, []string{"MinLength was increased"}},
		{"minlength decreased", Change{ChangeType: MinLengthDecreased, OldValue: "2", NewValue: "1"}, []string{"MinLength was decreased"}},
		{"maxlength added", Change{ChangeType: MaxLengthAdded, NewValue: "9"}, []string{"MaxLength constraint", "added"}},
		{"maxlength removed", Change{ChangeType: MaxLengthRemoved, OldValue: "9"}, []string{"MaxLength constraint", "removed"}},
		{"maxlength increased", Change{ChangeType: MaxLengthIncreased, OldValue: "8", NewValue: "9"}, []string{"MaxLength was increased"}},
		{"maxlength decreased", Change{ChangeType: MaxLengthDecreased, OldValue: "9", NewValue: "8"}, []string{"MaxLength was decreased"}},

		{"minitems added", Change{ChangeType: MinItemsAdded, NewValue: "1"}, []string{"MinItems constraint", "added"}},
		{"minitems removed", Change{ChangeType: MinItemsRemoved, OldValue: "1"}, []string{"MinItems constraint", "removed"}},
		{"minitems increased", Change{ChangeType: MinItemsIncreased, OldValue: "1", NewValue: "2"}, []string{"MinItems was increased"}},
		{"minitems decreased", Change{ChangeType: MinItemsDecreased, OldValue: "2", NewValue: "1"}, []string{"MinItems was decreased"}},
		{"maxitems added", Change{ChangeType: MaxItemsAdded, NewValue: "9"}, []string{"MaxItems constraint", "added"}},
		{"maxitems removed", Change{ChangeType: MaxItemsRemoved, OldValue: "9"}, []string{"MaxItems constraint", "removed"}},
		{"maxitems increased", Change{ChangeType: MaxItemsIncreased, OldValue: "8", NewValue: "9"}, []string{"MaxItems was increased"}},
		{"maxitems decreased", Change{ChangeType: MaxItemsDecreased, OldValue: "9", NewValue: "8"}, []string{"MaxItems was decreased"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.change.Description()
			require.NotEmpty(t, got)
			assert.NotContains(t, got, "Unknown change",
				"every declared ChangeType must have its own description")
			for _, want := range tt.contains {
				assert.Contains(t, got, want)
			}
		})
	}
}

// An unrecognised ChangeType still has to say something actionable, naming the
// path so the author can find the field even when kro cannot classify it.
func TestChangeDescriptionUnknownType(t *testing.T) {
	t.Parallel()
	got := Change{Path: "spec.mystery", ChangeType: ChangeType("SOMETHING_NEW")}.Description()
	assert.Contains(t, got, "Unknown change")
	assert.Contains(t, got, "spec.mystery",
		"the fallback must still name the path")
}
