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

package conversion_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/cel-go/cel"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/conversion"
	"github.com/kubernetes-sigs/kro/pkg/cel/library"
)

// TestKroTimeValuesRejectedAtRender pins the KREP-025 escape-hatch contract:
// a bare time.now()-derived value must NOT render into an object (that would
// be an implicit, requeue-less escape from the solver); string(...) is the
// only sanctioned exit.
func TestKroTimeValuesRejectedAtRender(t *testing.T) {
	env, err := cel.NewEnv(library.Time())
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	eval := func(expr string) (any, error) {
		ast, err := krocel.ParseAndCheck(env, expr)
		if err != nil {
			t.Fatalf("compile %q: %v", expr, err)
		}
		prog, err := env.Program(ast)
		if err != nil {
			t.Fatalf("program %q: %v", expr, err)
		}
		out, _, err := prog.Eval(map[string]any{library.TimeVarName: library.NewTimeValue(now)})
		if err != nil {
			return nil, err
		}
		return conversion.GoNativeType(out)
	}

	// Bare timestamp: rejected with guidance.
	if _, err := eval(`time.now()`); err == nil || !strings.Contains(err.Error(), "string(") {
		t.Fatalf("bare time.now() render: got err=%v, want string(...) guidance error", err)
	}
	// Bare duration: rejected with guidance.
	if _, err := eval(`time.now() - time.now()`); err == nil || !strings.Contains(err.Error(), "string(") {
		t.Fatalf("bare kro duration render: got err=%v, want string(...) guidance error", err)
	}
	// Explicit string(): the sanctioned escape hatch.
	got, err := eval(`string(time.now())`)
	if err != nil {
		t.Fatalf("string(time.now()) render: %v", err)
	}
	if got != "2026-09-17T12:00:00Z" {
		t.Fatalf("string(time.now()) = %v, want 2026-09-17T12:00:00Z", got)
	}
}
