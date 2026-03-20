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

package ast

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
)

func TestInspector_InspectionResults(t *testing.T) {
	tests := []struct {
		name          string
		resources     []string
		functions     []string
		expression    string
		wantResources []ResourceDependency
		wantFunctions []FunctionCall
	}{
		{
			name:       "simple eks cluster state check",
			resources:  []string{"eksCluster"},
			expression: `eksCluster.status.state == "ACTIVE"`,
			wantResources: []ResourceDependency{
				{ID: "eksCluster", Path: "eksCluster.status.state"},
			},
		},
		{
			name:       "simple bucket name check",
			resources:  []string{"bucket"},
			expression: `bucket.spec.name == "my-bucket" && bucket.metadata.name == bucket.spec.name`,
			wantResources: []ResourceDependency{
				{ID: "bucket", Path: "bucket.metadata.name"},
				{ID: "bucket", Path: "bucket.spec.name"},
				{ID: "bucket", Path: "bucket.spec.name"},
			},
		},
		{
			name:       "bucket name with function",
			resources:  []string{"bucket"},
			functions:  []string{"toLower"},
			expression: `toLower(bucket.name)`,
			wantResources: []ResourceDependency{
				{ID: "bucket", Path: "bucket.name"},
			},
			wantFunctions: []FunctionCall{
				{Name: "toLower", Arguments: []string{"bucket.name"}},
			},
		},
		{
			name:       "deployment replicas with function",
			resources:  []string{"deployment"},
			functions:  []string{"max"},
			expression: `max(deployment.spec.replicas, 5)`,
			wantResources: []ResourceDependency{
				{ID: "deployment", Path: "deployment.spec.replicas"},
			},
			wantFunctions: []FunctionCall{
				{Name: "max", Arguments: []string{"deployment.spec.replicas", "5"}},
			},
		},
		{
			name:       "OR and index operators simple",
			resources:  []string{"list", "flags"},
			functions:  []string{},
			expression: `list[0] || flags["enabled"]`,
			wantResources: []ResourceDependency{
				{ID: "list", Path: "list"},
				{ID: "flags", Path: "flags"},
			},
		},
		{
			name:      "mixed constant types",
			resources: []string{},
			functions: []string{"process"},
			expression: `process(
				b"bytes123",         // BytesValue
				3.14,               // DoubleValue
				42u,                // Uint64Value
				null               // NullValue
			)`,
			wantResources: nil,
			wantFunctions: []FunctionCall{
				{Name: "process", Arguments: []string{`b"\142\171\164\145\163\061\062\063"`, "3.14", "42u", "null"}},
			},
		},
		{
			name:       "test operator string conversion",
			resources:  []string{"list", "conditions"},
			functions:  []string{"validate"},
			expression: `validate(conditions.ready || conditions.initialized && list[3])`,
			wantResources: []ResourceDependency{
				{ID: "list", Path: "list"},
				{ID: "conditions", Path: "conditions.ready"},
				{ID: "conditions", Path: "conditions.initialized"},
			},
			wantFunctions: []FunctionCall{
				{Name: "validate", Arguments: []string{
					"(conditions.ready || (conditions.initialized && list[3]))",
				}},
			},
		},
		{
			name:       "eks and nodegroup check",
			resources:  []string{"eksCluster", "nodeGroup"},
			expression: `eksCluster.spec.version == nodeGroup.spec.version`,
			wantResources: []ResourceDependency{
				{ID: "eksCluster", Path: "eksCluster.spec.version"},
				{ID: "nodeGroup", Path: "nodeGroup.spec.version"},
			},
		},
		{
			name:       "deployment and cluster version",
			resources:  []string{"deployment", "eksCluster"},
			expression: `deployment.metadata.namespace == "default" && eksCluster.spec.version == "1.31"`,
			wantResources: []ResourceDependency{
				{ID: "deployment", Path: "deployment.metadata.namespace"},
				{ID: "eksCluster", Path: "eksCluster.spec.version"},
			},
		},
		{
			name:       "eks name and bucket prefix",
			resources:  []string{"eksCluster", "bucket"},
			functions:  []string{"concat", "toLower"},
			expression: `concat(toLower(eksCluster.spec.name), "-", bucket.spec.name)`,
			wantResources: []ResourceDependency{
				{ID: "eksCluster", Path: "eksCluster.spec.name"},
				{ID: "bucket", Path: "bucket.spec.name"},
			},
			wantFunctions: []FunctionCall{
				{Name: "concat", Arguments: []string{
					"toLower(eksCluster.spec.name)",
					`"-"`,
					"bucket.spec.name",
				}},
				{Name: "toLower", Arguments: []string{"eksCluster.spec.name"}},
			},
		},
		{
			name:       "instances count",
			resources:  []string{"instances"},
			functions:  []string{"count"},
			expression: `count(instances) > 0`,
			wantResources: []ResourceDependency{
				{ID: "instances", Path: "instances"},
			},
			wantFunctions: []FunctionCall{
				{Name: "count", Arguments: []string{"instances"}},
			},
		},
		{
			name:      "complex expressions",
			resources: []string{"fargateProfile", "eksCluster"},
			functions: []string{"contains", "count"},
			expression: `contains(fargateProfile.spec.subnets, "subnet-123") &&
                count(fargateProfile.spec.selectors) <= 5 &&
                eksCluster.status.state == "ACTIVE"`,
			wantResources: []ResourceDependency{
				{ID: "fargateProfile", Path: "fargateProfile.spec.subnets"},
				{ID: "fargateProfile", Path: "fargateProfile.spec.selectors"},
				{ID: "eksCluster", Path: "eksCluster.status.state"},
			},
			wantFunctions: []FunctionCall{
				{Name: "contains", Arguments: []string{"fargateProfile.spec.subnets", `"subnet-123"`}},
				{Name: "count", Arguments: []string{"fargateProfile.spec.selectors"}},
			},
		},
		{
			name:      "complex security group validation",
			resources: []string{"securityGroup", "vpc"},
			functions: []string{"concat", "contains", "map"},
			expression: `securityGroup.spec.vpcID == vpc.status.vpcID &&
                securityGroup.spec.rules.all(r,
                    contains(map(r.ipRanges, range, concat(range.cidr, "/", range.description)),
                        "0.0.0.0/0"))`,
			wantResources: []ResourceDependency{
				{ID: "securityGroup", Path: "securityGroup.spec.vpcID"},
				{ID: "securityGroup", Path: "securityGroup.spec.rules"},
				{ID: "vpc", Path: "vpc.status.vpcID"},
			},
			wantFunctions: []FunctionCall{
				{Name: "concat", Arguments: []string{"range.cidr", `"/"`, "range.description"}},
				{Name: "contains", Arguments: []string{
					"map(r.ipRanges, range, concat(range.cidr, \"/\", range.description))",
					`"0.0.0.0/0"`,
				}},
				{Name: "map", Arguments: []string{"r.ipRanges", "range", "concat(range.cidr, \"/\", range.description)"}},
				{Name: "filter", Arguments: []string{
					"securityGroup.spec.rules",
					"(@result && contains(map(r.ipRanges, range, concat(range.cidr, \"/\", range.description)), \"0.0.0.0/0\"))",
					"@result",
				}},
			},
		},
		{
			name:      "eks cluster validation",
			resources: []string{"eksCluster", "nodeGroups", "iamRole", "vpc"},
			functions: []string{"filter", "contains", "timeAgo"},
			expression: `eksCluster.status.state == "ACTIVE" &&
				duration(timeAgo(eksCluster.status.createdAt)) > duration("24h") &&
				size(nodeGroups.filter(ng,
					ng.status.state == "ACTIVE" &&
					contains(ng.labels, "environment"))) >= 1 &&
				contains(map(iamRole.policies, p, p.actions), "eks:*") &&
				size(vpc.subnets.filter(s, s.isPrivate)) >= 2`,
			wantResources: []ResourceDependency{
				{ID: "eksCluster", Path: "eksCluster.status.state"},
				{ID: "eksCluster", Path: "eksCluster.status.createdAt"},
				{ID: "nodeGroups", Path: "nodeGroups"},
				{ID: "iamRole", Path: "iamRole.policies"},
				{ID: "vpc", Path: "vpc.subnets"},
			},
			wantFunctions: []FunctionCall{
				{Name: "contains", Arguments: []string{"ng.labels", `"environment"`}},
				{Name: "contains", Arguments: []string{"map(iamRole.policies, p, p.actions)", `"eks:*"`}},
				{Name: "createList", Arguments: []string{"[]"}}, // AccuInit for nodeGroups.filter
				{Name: "createList", Arguments: []string{"[ng]"}},
				{Name: "createList", Arguments: []string{"[]"}}, // AccuInit for vpc.subnets.filter
				{Name: "createList", Arguments: []string{"[s]"}},
				{Name: "filter", Arguments: []string{
					"nodeGroups",
					"(((ng.status.state == \"ACTIVE\") && contains(ng.labels, \"environment\")) ? (@result + [ng]) : @result)",
					"@result",
				}},
				{Name: "filter", Arguments: []string{
					"vpc.subnets",
					"(s.isPrivate ? (@result + [s]) : @result)",
					"@result",
				}},
				{Name: "timeAgo", Arguments: []string{"eksCluster.status.createdAt"}},
			},
		},
		{
			name:      "validate order and inventory",
			resources: []string{"order", "product", "customer", "inventory"},
			functions: []string{"validateAddress", "calculateTax"},
			expression: `order.total > 0 &&
				order.items.all(item,
					product.id == item.productId &&
					inventory.stock[item.productId] >= item.quantity
				) &&
				validateAddress(customer.shippingAddress) &&
				calculateTax(order.total, customer.address.zipCode) > 0 || true`,
			wantResources: []ResourceDependency{
				{ID: "order", Path: "order.total"},
				{ID: "order", Path: "order.total"},
				{ID: "order", Path: "order.items"},
				{ID: "product", Path: "product.id"},
				{ID: "inventory", Path: "inventory.stock"},
				{ID: "customer", Path: "customer.shippingAddress"},
				{ID: "customer", Path: "customer.address.zipCode"},
			},
			wantFunctions: []FunctionCall{
				{Name: "calculateTax", Arguments: []string{"order.total", "customer.address.zipCode"}},
				{Name: "filter", Arguments: []string{
					"order.items",
					"(@result && ((product.id == item.productId) && (inventory.stock[item.productId] >= item.quantity)))",
					"@result",
				}},
				{Name: "validateAddress", Arguments: []string{"customer.shippingAddress"}},
			},
		},
		{
			name:       "filter with explicit condition",
			resources:  []string{"pods"},
			functions:  []string{},
			expression: `pods.filter(p, p.status == "Running")`,
			wantResources: []ResourceDependency{
				{ID: "pods", Path: "pods"},
			},
			wantFunctions: []FunctionCall{
				{Name: "createList", Arguments: []string{"[]"}}, // AccuInit
				{Name: "createList", Arguments: []string{"[p]"}},
				{Name: "filter", Arguments: []string{
					"pods",
					"((p.status == \"Running\") ? (@result + [p]) : @result)",
					"@result",
				}},
			},
		},
		{
			name:          "create message struct",
			resources:     []string{},
			functions:     []string{"createPod"},
			expression:    `createPod(Pod{metadata: {name: "test", labels: {"app": "web"}}, spec: {containers: [{name: "main", image: "nginx"}]}})`,
			wantResources: nil,
			wantFunctions: []FunctionCall{
				{Name: "createList", Arguments: []string{`[{name: "main", image: "nginx"}]`}},
				{Name: "createPod", Arguments: []string{
					`Pod{metadata: {name: "test", labels: {"app": "web"}}, spec: {containers: [{name: "main", image: "nginx"}]}}`,
				}},
			},
		},
		{
			name:      "create map with different key types",
			resources: []string{},
			functions: []string{"processMap"},
			expression: `processMap({
				"string-key": 123,
				42: "number-key",
				true: "bool-key"
			})`,
			wantResources: nil,
			wantFunctions: []FunctionCall{
				{Name: "processMap", Arguments: []string{
					`{"string-key": 123, 42: "number-key", true: "bool-key"}`,
				}},
			},
		},
		{
			name:      "message with nested structs",
			resources: []string{},
			functions: []string{"validate"},
			expression: `validate(Container{
				resource: Resource{cpu: "100m", memory: "256Mi"},
				env: {
					"DB_HOST": "localhost",
					"DB_PORT": "5432"
				}
			})`,
			wantResources: nil,
			wantFunctions: []FunctionCall{
				{Name: "validate", Arguments: []string{
					`Container{resource: Resource{cpu: "100m", memory: "256Mi"}, env: {"DB_HOST": "localhost", "DB_PORT": "5432"}}`,
				}},
			},
		},
		{
			name:       "simple optional check",
			resources:  []string{"bucket"},
			expression: `bucket.?spec.name == "my-bucket"`,
			wantResources: []ResourceDependency{
				// for optionals, we can only depend on the known object, not on the path thereafter (as its optional)
				{ID: "bucket", Path: "bucket"},
			},
		},
		{
			name:       "format statement powered by list",
			resources:  []string{"serviceAccount", "configMap", "schema"},
			expression: `"%s:%s".format([schema.metadata.namespace, serviceAccount.metadata.name])`,
			wantFunctions: []FunctionCall{
				{Name: `"%s:%s".format`},
				{Name: "createList", Arguments: []string{"[schema.metadata.namespace, serviceAccount.metadata.name]"}},
			},
			wantResources: []ResourceDependency{
				{ID: "serviceAccount", Path: "serviceAccount.metadata.name"},
				{ID: "schema", Path: "schema.metadata.namespace"},
			},
		},
		{
			name:       "simple list literal",
			resources:  []string{},
			expression: `[1, 2, 3]`,
			wantFunctions: []FunctionCall{
				{Name: "createList", Arguments: []string{"[1, 2, 3]"}},
			},
		},

		{
			name:       "nested list literal",
			resources:  []string{},
			expression: `[[1, 2], ["a", "b"]]`,
			wantFunctions: []FunctionCall{
				{Name: "createList", Arguments: []string{"[1, 2]"}},
				{Name: "createList", Arguments: []string{"[\"a\", \"b\"]"}},
				{Name: "createList", Arguments: []string{"[[1, 2], [\"a\", \"b\"]]"}},
			},
		},

		{
			name:       "list with struct elements",
			resources:  []string{},
			expression: `[{a: 1}, {b: 2}]`,
			wantFunctions: []FunctionCall{
				{Name: "createList", Arguments: []string{"[{a: 1}, {b: 2}]"}},
			},
		},

		{
			name:       "list containing function calls",
			resources:  []string{},
			functions:  []string{"toLower", "hash"},
			expression: `[toLower("A"), hash("x")]`,
			wantFunctions: []FunctionCall{
				{Name: "toLower", Arguments: []string{`"A"`}},
				{Name: "hash", Arguments: []string{`"x"`}},
				{Name: "createList", Arguments: []string{"[toLower(\"A\"), hash(\"x\")]"}},
			},
		},
		{
			name:       "list with optional access",
			resources:  []string{"bucket"},
			expression: `[bucket.?spec.name, "x"]`,
			wantResources: []ResourceDependency{
				{ID: "bucket", Path: "bucket"},
			},
			wantFunctions: []FunctionCall{
				{
					Name:      "createList",
					Arguments: []string{`[_?._(bucket, "spec").name, "x"]`},
				},
			},
		},
		{
			name:       "list inside binary operator",
			resources:  []string{"cfg"},
			expression: `size([cfg.a, cfg.b]) > 1`,
			// no "size" – CEL already provides it
			functions: []string{},
			wantResources: []ResourceDependency{
				{ID: "cfg", Path: "cfg.a"},
				{ID: "cfg", Path: "cfg.b"},
			},
			wantFunctions: []FunctionCall{
				// size() will NOT be recorded, because it’s built-in
				{Name: "createList", Arguments: []string{"[cfg.a, cfg.b]"}},
			},
		},
		{
			name:       "map with resources",
			resources:  []string{"pod"},
			expression: `{"name": pod.metadata.name}`,
			wantResources: []ResourceDependency{
				{ID: "pod", Path: "pod.metadata.name"},
			},
		},
		{
			name:       "struct with resources",
			resources:  []string{"deployment"},
			expression: `Config{replicas: deployment.spec.replicas}`,
			wantResources: []ResourceDependency{
				{ID: "deployment", Path: "deployment.spec.replicas"},
			},
		},
		{
			name:       "nested maps with resources",
			resources:  []string{"app", "db"},
			expression: `{"app": {"name": app.metadata.name, "version": app.spec.version}, "db": {"host": db.spec.host}}`,
			wantResources: []ResourceDependency{
				{ID: "app", Path: "app.metadata.name"},
				{ID: "app", Path: "app.spec.version"},
				{ID: "db", Path: "db.spec.host"},
			},
		},
		{
			name:       "map with dynamic key from resource",
			resources:  []string{"config"},
			expression: `{config.metadata.name: config.spec.value}`,
			wantResources: []ResourceDependency{
				{ID: "config", Path: "config.metadata.name"},
				{ID: "config", Path: "config.spec.value"},
			},
		},
		{
			name:       "map inside list",
			resources:  []string{"svc"},
			expression: `[{"port": svc.spec.ports[0].port}]`,
			wantResources: []ResourceDependency{
				{ID: "svc", Path: "svc.spec.ports"},
			},
			wantFunctions: []FunctionCall{
				{Name: "createList", Arguments: []string{`[{"port": svc.spec.ports[0].port}]`}},
			},
		},
		{
			name:       "map with function calls in values",
			resources:  []string{"user"},
			functions:  []string{"toLower", "hash"},
			expression: `{"username": toLower(user.name), "hash": hash(user.password)}`,
			wantResources: []ResourceDependency{
				{ID: "user", Path: "user.name"},
				{ID: "user", Path: "user.password"},
			},
			wantFunctions: []FunctionCall{
				{Name: "hash", Arguments: []string{"user.password"}},
				{Name: "toLower", Arguments: []string{"user.name"}},
			},
		},
		{
			name:       "resource id collides with function namespace",
			resources:  []string{"random"},
			functions:  []string{"random.seededString"},
			expression: `random.status.field == "x" && random.seededString(10, "abc") != ""`,
			wantResources: []ResourceDependency{
				{ID: "random", Path: "random.status.field"},
			},
			wantFunctions: []FunctionCall{
				{Name: "random.seededString"},
			},
		},
		{
			name:       "duplicate resource references are NOT deduplicated",
			resources:  []string{"deployment"},
			expression: `deployment.spec.replicas + deployment.spec.replicas + deployment.status.replicas`,
			wantResources: []ResourceDependency{
				// Inspector reports each occurrence - no deduplication at this level
				{ID: "deployment", Path: "deployment.spec.replicas"},
				{ID: "deployment", Path: "deployment.spec.replicas"},
				{ID: "deployment", Path: "deployment.status.replicas"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inspector, err := testInspector(tt.resources, tt.functions)
			if err != nil {
				t.Fatalf("Failed to create inspector: %v", err)
			}

			got, err := inspector.Inspect(tt.expression)
			if err != nil {
				t.Fatalf("Inspect() error = %v", err)
			}

			sortDependencies := func(a, b ResourceDependency) int {
				return strings.Compare(a.Path, b.Path)
			}
			sortFunctions := func(a, b FunctionCall) int {
				return strings.Compare(a.Name, b.Name)
			}

			slices.SortFunc(got.ResourceDependencies, sortDependencies)
			slices.SortFunc(tt.wantResources, sortDependencies)
			if !reflect.DeepEqual(got.ResourceDependencies, tt.wantResources) {
				t.Errorf("ResourceDependencies = %v, want %v", got.ResourceDependencies, tt.wantResources)
			}

			slices.SortFunc(got.FunctionCalls, sortFunctions)
			slices.SortFunc(tt.wantFunctions, sortFunctions)
			if !reflect.DeepEqual(got.FunctionCalls, tt.wantFunctions) {
				t.Errorf("FunctionCalls = %v, want %v", got.FunctionCalls, tt.wantFunctions)
			}
		})
	}
}

func TestInspector_UnknownResourcesAndCalls(t *testing.T) {
	tests := []struct {
		name           string
		resources      []string
		functions      []string
		expression     string
		wantResources  []ResourceDependency
		wantFunctions  []FunctionCall
		wantUnknownRes []UnknownResource
	}{
		{
			name:          "method call on unknown resource",
			resources:     []string{"list"},
			expression:    `unknownResource.someMethod(42)`,
			wantResources: nil,
			wantFunctions: []FunctionCall{
				{Name: "unknownResource.someMethod"},
			},
			wantUnknownRes: []UnknownResource{
				{ID: "unknownResource", Path: "unknownResource"},
			},
		},
		{
			name:          "chained method calls on unknown resource",
			resources:     []string{},
			expression:    `unknown.method1().method2(123)`,
			wantResources: nil,
			wantFunctions: []FunctionCall{
				{Name: "unknown.method1"},
				{Name: "unknown.method1().method2"},
			},
			wantUnknownRes: []UnknownResource{
				{ID: "unknown", Path: "unknown"},
			},
		},
		{
			name:      "filter with multiple conditions",
			resources: []string{"instances"},
			// note that `i` is not declared as a resource, but it's not an unknown resource
			// either, it's a loop variable.
			expression: `instances.filter(i,
                i.state == 'running' &&
                i.type == 't2.micro'
            )`,
			wantResources: []ResourceDependency{
				{ID: "instances", Path: "instances"},
			},
			wantFunctions: []FunctionCall{
				{Name: "createList"},
				{Name: "createList"}, // AccuInit empty list
				{Name: "filter"},
			},
		},
		{
			name:      "ambiguous i usage - both resource and loop var",
			resources: []string{"instances", "i"}, // 'i' is a declared resource
			expression: `i.status == "ready" &&
				instances.filter(i,   // reusing 'i' in filter
					i.state == 'running'
				)`,
			wantResources: []ResourceDependency{
				{ID: "i", Path: "i.status"},
				{ID: "instances", Path: "instances"},
			},
			wantFunctions: []FunctionCall{
				{Name: "createList"},
				{Name: "createList"}, // AccuInit empty list
				{Name: "filter"},
			},
			wantUnknownRes: nil,
		},
		{
			name:       "test target function chaining",
			resources:  []string{"bucket"},
			functions:  []string{"processItems", "validate"},
			expression: `processItems(bucket).validate()`,
			wantResources: []ResourceDependency{
				{ID: "bucket", Path: "bucket"},
			},
			wantFunctions: []FunctionCall{
				{Name: "processItems"},
				{Name: "processItems(bucket).validate"},
			},
		},
		{
			name:      "sortBy macro internal identifiers are not flagged as unknown",
			resources: []string{"configs"},
			// sortBy expands into nested comprehensions with internal variables
			// like @__sortBy_input__ and reused iterVar "c". The inspector must
			// recognise all of these as internal/loop variables.
			expression: `configs.sortBy(c, c.data.priority).map(c, c.metadata.name)`,
			wantResources: []ResourceDependency{
				{ID: "configs", Path: "configs"},
			},
			wantFunctions: []FunctionCall{
				// macro expansion produces multiple comprehensions and internal calls
				{Name: "@__sortBy_input__.@sortByAssociatedKeys"},
				{Name: "createList"},
				{Name: "createList"},
				{Name: "createList"},
				{Name: "createList"},
				{Name: "createList"},
				{Name: "filter"},
				{Name: "filter"},
				{Name: "filter"},
			},
			wantUnknownRes: nil,
		},
		{
			name:          "test unknown function with target",
			resources:     []string{},
			functions:     []string{},
			expression:    `result.unknownFn().anotherUnknownFn()`,
			wantResources: nil,
			wantFunctions: []FunctionCall{
				{Name: "result.unknownFn"},
				{Name: "result.unknownFn().anotherUnknownFn"},
			},
			wantUnknownRes: []UnknownResource{
				{ID: "result", Path: "result"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inspector, err := testInspector(tt.resources, tt.functions)
			if err != nil {
				t.Fatalf("Failed to create inspector: %v", err)
			}

			got, err := inspector.Inspect(tt.expression)
			if err != nil {
				t.Fatalf("Inspect() error = %v", err)
			}

			// Sort for stable comparison
			sortDependencies := func(deps []ResourceDependency) {
				sort.Slice(deps, func(i, j int) bool {
					return deps[i].Path < deps[j].Path
				})
			}

			sortFunctions := func(funcs []FunctionCall) {
				sort.Slice(funcs, func(i, j int) bool {
					return funcs[i].Name < funcs[j].Name
				})
			}

			sortUnknownResources := func(res []UnknownResource) {
				sort.Slice(res, func(i, j int) bool {
					return res[i].Path < res[j].Path
				})
			}

			sortDependencies(got.ResourceDependencies)
			sortDependencies(tt.wantResources)
			sortFunctions(got.FunctionCalls)
			sortFunctions(tt.wantFunctions)
			sortUnknownResources(got.UnknownResources)
			sortUnknownResources(tt.wantUnknownRes)

			if !reflect.DeepEqual(got.ResourceDependencies, tt.wantResources) {
				t.Errorf("ResourceDependencies = %v, want %v", got.ResourceDependencies, tt.wantResources)
			}

			// Only check function names, not arguments
			gotFuncNames := make([]string, len(got.FunctionCalls))
			wantFuncNames := make([]string, len(tt.wantFunctions))
			for i, f := range got.FunctionCalls {
				gotFuncNames[i] = f.Name
			}
			for i, f := range tt.wantFunctions {
				wantFuncNames[i] = f.Name
			}
			sort.Strings(gotFuncNames)
			sort.Strings(wantFuncNames)

			if !reflect.DeepEqual(gotFuncNames, wantFuncNames) {
				t.Errorf("Function names = %v, want %v", gotFuncNames, wantFuncNames)
			}

			if !reflect.DeepEqual(got.UnknownResources, tt.wantUnknownRes) {
				t.Errorf("UnknownResources = %v, want %v", got.UnknownResources, tt.wantUnknownRes)
			}
		})
	}
}

// TestNewInspectorWithEnv_CustomFunctionsNotResources verifies that when
// using NewInspectorWithEnv (the production constructor), kro's custom CEL
// functions are automatically recognised as functions — not misclassified
// as resource dependencies or unknown functions.
func TestNewInspectorWithEnv_CustomFunctionsNotResources(t *testing.T) {
	tests := []struct {
		name           string
		resources      []string
		expression     string
		wantResources  []ResourceDependency
		wantFunctions  []FunctionCall
		wantUnknownFns []UnknownFunction
		wantUnknownRes []UnknownResource
	}{
		{
			name:       "random.seededString is a function, not a resource",
			resources:  []string{"schema"},
			expression: `random.seededString(10, schema.metadata.uid)`,
			wantResources: []ResourceDependency{
				{ID: "schema", Path: "schema.metadata.uid"},
			},
			wantFunctions: []FunctionCall{
				{Name: "random.seededString"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: nil,
		},
		{
			name:       "base64.encode is a function, not a resource",
			resources:  []string{"secret"},
			expression: `base64.encode(bytes(secret.data.password))`,
			wantResources: []ResourceDependency{
				{ID: "secret", Path: "secret.data.password"},
			},
			wantFunctions: []FunctionCall{
				{Name: "base64.encode"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: nil,
		},
		{
			name:       "json.marshal is a function, not a resource",
			resources:  []string{"config"},
			expression: `json.marshal(config.spec.data)`,
			wantResources: []ResourceDependency{
				{ID: "config", Path: "config.spec.data"},
			},
			wantFunctions: []FunctionCall{
				{Name: "json.marshal"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: nil,
		},
		{
			name:       "json.unmarshal is a function, not a resource",
			resources:  []string{},
			expression: `json.unmarshal('{"key":"val"}')`,
			wantFunctions: []FunctionCall{
				{Name: "json.unmarshal"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: nil,
		},
		{
			name:       "resource named 'random' coexists with random.seededString",
			resources:  []string{"random"},
			expression: `random.status.ready == true && random.seededString(5, "seed") != ""`,
			wantResources: []ResourceDependency{
				{ID: "random", Path: "random.status.ready"},
			},
			wantFunctions: []FunctionCall{
				{Name: "random.seededString"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: nil,
		},
		{
			name:       "resource named 'json' coexists with json.marshal",
			resources:  []string{"json"},
			expression: `json.status.phase == "Ready" && json.marshal(json.spec.data) != ""`,
			wantResources: []ResourceDependency{
				{ID: "json", Path: "json.status.phase"},
				{ID: "json", Path: "json.spec.data"},
			},
			wantFunctions: []FunctionCall{
				{Name: "json.marshal"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: nil,
		},
		{
			name:       "multiple custom functions in one expression",
			resources:  []string{"schema"},
			expression: `json.marshal(json.unmarshal(random.seededString(5, schema.metadata.uid)))`,
			wantResources: []ResourceDependency{
				{ID: "schema", Path: "schema.metadata.uid"},
			},
			wantFunctions: []FunctionCall{
				{Name: "json.marshal"},
				{Name: "json.unmarshal"},
				{Name: "random.seededString"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: nil,
		},
		{
			name:       "undeclared resource is reported as unknown",
			resources:  []string{"schema"},
			expression: `deployment.spec.replicas > 0 && schema.spec.name != ""`,
			wantResources: []ResourceDependency{
				{ID: "schema", Path: "schema.spec.name"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: []UnknownResource{
				{ID: "deployment", Path: "deployment.spec.replicas"},
			},
		},
		{
			name:           "multiple undeclared resources",
			resources:      []string{},
			expression:     `service.spec.port == deployment.spec.containerPort`,
			wantUnknownFns: nil,
			wantUnknownRes: []UnknownResource{
				{ID: "deployment", Path: "deployment.spec.containerPort"},
				{ID: "service", Path: "service.spec.port"},
			},
		},
		{
			name:       "undeclared resource passed to custom function",
			resources:  []string{},
			expression: `json.marshal(unknown.spec.config)`,
			wantFunctions: []FunctionCall{
				{Name: "json.marshal"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: []UnknownResource{
				{ID: "unknown", Path: "unknown.spec.config"},
			},
		},
		{
			name:       "mix of known resources, unknown resources, and custom functions",
			resources:  []string{"schema", "configMap"},
			expression: `random.seededString(5, schema.metadata.uid) + missing.spec.suffix + configMap.data.key`,
			wantResources: []ResourceDependency{
				{ID: "configMap", Path: "configMap.data.key"},
				{ID: "schema", Path: "schema.metadata.uid"},
			},
			wantFunctions: []FunctionCall{
				{Name: "random.seededString"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: []UnknownResource{
				{ID: "missing", Path: "missing.spec.suffix"},
			},
		},
		{
			name:       "method call on undeclared resource reports unknown resource and function",
			resources:  []string{},
			expression: `notAResource.customMethod(42)`,
			wantFunctions: []FunctionCall{
				{Name: "notAResource.customMethod"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: []UnknownResource{
				{ID: "notAResource", Path: "notAResource"},
			},
		},
		// CEL stdlib and K8s library functions
		{
			name:       "size is not an unknown function",
			resources:  []string{"pods"},
			expression: `size(pods.items) > 0`,
			wantResources: []ResourceDependency{
				{ID: "pods", Path: "pods.items"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: nil,
		},
		{
			name:       "duration and timestamp are not unknown functions",
			resources:  []string{"schema"},
			expression: `duration("24h") > duration(schema.spec.timeout)`,
			wantResources: []ResourceDependency{
				{ID: "schema", Path: "schema.spec.timeout"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: nil,
		},
		{
			name:       "type conversion functions are not unknown",
			resources:  []string{"config"},
			expression: `int(config.spec.replicas) + int(config.spec.extra) > 0`,
			wantResources: []ResourceDependency{
				{ID: "config", Path: "config.spec.extra"},
				{ID: "config", Path: "config.spec.replicas"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: nil,
		},
		{
			name:       "string member functions are not unknown",
			resources:  []string{"svc"},
			expression: `svc.metadata.name.startsWith("prod-") && svc.spec.type.contains("Load")`,
			wantResources: []ResourceDependency{
				{ID: "svc", Path: "svc.metadata.name"},
				{ID: "svc", Path: "svc.spec.type"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: nil,
		},
		{
			name:       "lists.range is not an unknown function",
			resources:  []string{},
			expression: `lists.range(3)`,
			wantFunctions: []FunctionCall{
				{Name: "lists.range"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: nil,
		},
		{
			name:       "lists.sum is not an unknown function",
			resources:  []string{"items"},
			expression: `items.prices.sum() > 100`,
			wantResources: []ResourceDependency{
				{ID: "items", Path: "items.prices"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: nil,
		},
		{
			name:       "lists.isSorted is not an unknown function",
			resources:  []string{"schema"},
			expression: `schema.spec.values.isSorted()`,
			wantResources: []ResourceDependency{
				{ID: "schema", Path: "schema.spec.values"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: nil,
		},
		{
			name:       "quantity is not an unknown function",
			resources:  []string{"pvc"},
			expression: `quantity(pvc.spec.resources.requests.storage).isGreaterThan(quantity("10Gi"))`,
			wantResources: []ResourceDependency{
				{ID: "pvc", Path: "pvc.spec.resources.requests.storage"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: nil,
		},
		{
			name:       "isQuantity is not an unknown function",
			resources:  []string{"schema"},
			expression: `isQuantity(schema.spec.memory) ? quantity(schema.spec.memory) : quantity("512Mi")`,
			wantResources: []ResourceDependency{
				{ID: "schema", Path: "schema.spec.memory"},
				{ID: "schema", Path: "schema.spec.memory"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: nil,
		},
		{
			name:       "has macro is not an unknown function",
			resources:  []string{"deploy"},
			expression: `has(deploy.spec.replicas)`,
			wantResources: []ResourceDependency{
				{ID: "deploy", Path: "deploy.spec.replicas"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: nil,
		},
		{
			name:       "map and filter comprehensions with stdlib do not produce unknowns",
			resources:  []string{"pods"},
			expression: `pods.items.filter(p, p.status == "Running").size() > 0`,
			wantResources: []ResourceDependency{
				{ID: "pods", Path: "pods.items"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: nil,
		},
		{
			name:       "cel.bind variable is not reported as unknown identifier",
			resources:  []string{"schema"},
			expression: `cel.bind(n, schema.spec.count * 2, n + n)`,
			wantResources: []ResourceDependency{
				{ID: "schema", Path: "schema.spec.count"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: nil,
		},
		{
			name:       "nested cel.bind variables are not reported as unknown identifiers",
			resources:  []string{"schema"},
			expression: `cel.bind(a, schema.spec.x + 1, cel.bind(b, a * 2, a + b))`,
			wantResources: []ResourceDependency{
				{ID: "schema", Path: "schema.spec.x"},
			},
			wantUnknownFns: nil,
			wantUnknownRes: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decls := make([]cel.EnvOption, 0, len(tt.resources))
			for _, r := range tt.resources {
				decls = append(decls, cel.Variable(r, cel.AnyType))
			}
			env, err := krocel.DefaultEnvironment(krocel.WithCustomDeclarations(decls))
			if err != nil {
				t.Fatalf("Failed to create CEL environment: %v", err)
			}

			inspector := NewInspectorWithEnv(env, tt.resources)
			got, err := inspector.Inspect(tt.expression)
			if err != nil {
				t.Fatalf("Inspect() error = %v", err)
			}

			// Check resource dependencies
			sortDeps := func(a, b ResourceDependency) int {
				return strings.Compare(a.Path, b.Path)
			}
			slices.SortFunc(got.ResourceDependencies, sortDeps)
			slices.SortFunc(tt.wantResources, sortDeps)
			if !reflect.DeepEqual(got.ResourceDependencies, tt.wantResources) {
				t.Errorf("ResourceDependencies = %v, want %v", got.ResourceDependencies, tt.wantResources)
			}

			// Check function calls by name (env.Functions() may add builtins;
			// we only assert that the expected custom functions are present)
			gotNames := map[string]bool{}
			for _, f := range got.FunctionCalls {
				gotNames[f.Name] = true
			}
			for _, want := range tt.wantFunctions {
				if !gotNames[want.Name] {
					t.Errorf("expected FunctionCall %q not found in %v", want.Name, got.FunctionCalls)
				}
			}

			// Check unknown functions
			sortUnknownFns := func(a, b UnknownFunction) int {
				return strings.Compare(a.Name, b.Name)
			}
			slices.SortFunc(got.UnknownFunctions, sortUnknownFns)
			slices.SortFunc(tt.wantUnknownFns, sortUnknownFns)
			if !reflect.DeepEqual(got.UnknownFunctions, tt.wantUnknownFns) {
				t.Errorf("UnknownFunctions = %v, want %v", got.UnknownFunctions, tt.wantUnknownFns)
			}

			// Check unknown resources
			sortUnknownRes := func(a, b UnknownResource) int {
				return strings.Compare(a.Path, b.Path)
			}
			slices.SortFunc(got.UnknownResources, sortUnknownRes)
			slices.SortFunc(tt.wantUnknownRes, sortUnknownRes)
			if !reflect.DeepEqual(got.UnknownResources, tt.wantUnknownRes) {
				t.Errorf("UnknownResources = %v, want %v", got.UnknownResources, tt.wantUnknownRes)
			}
		})
	}
}

func Test_InvalidExpression(t *testing.T) {
	_ = NewInspectorWithEnv(nil, []string{})

	inspector, err := testInspector([]string{}, []string{})
	if err != nil {
		t.Fatalf("Failed to create inspector: %v", err)
	}
	_, err = inspector.Inspect("invalid expression ######")
	if err == nil {
		t.Errorf("Expected error")
	}
}

func testInspector(resources []string, functions []string) (*Inspector, error) {
	decls := make([]cel.EnvOption, 0, len(resources)+len(functions))
	resourceMap := make(map[string]struct{})
	functionMap := make(map[string]struct{})

	for _, r := range resources {
		decls = append(decls, cel.Variable(r, cel.AnyType))
		resourceMap[r] = struct{}{}
	}

	for _, fn := range functions {
		decls = append(decls, cel.Function(fn,
			cel.Overload(fn+"_any", []*cel.Type{cel.AnyType}, cel.AnyType)))
		functionMap[fn] = struct{}{}
	}

	env, err := krocel.DefaultEnvironment(krocel.WithCustomDeclarations(decls))
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %v", err)
	}

	return &Inspector{
		env:       env,
		resources: resourceMap,
		functions: functionMap,
		loopVars:  make(map[string]struct{}),
	}, nil
}
