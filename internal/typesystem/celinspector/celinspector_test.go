// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.
package celinspector

import (
	"reflect"
	"sort"
	"testing"
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
				{Name: "eksCluster", Path: "eksCluster.status.state"},
			},
		},
		{
			name:       "simple bucket name check",
			resources:  []string{"bucket"},
			expression: `bucket.spec.name == "my-bucket" && bucket.metadata.name == bucket.spec.name`,
			wantResources: []ResourceDependency{
				{Name: "bucket", Path: "bucket.metadata.name"},
				{Name: "bucket", Path: "bucket.spec.name"},
				{Name: "bucket", Path: "bucket.spec.name"},
			},
		},

		{
			name:       "bucket name with function",
			resources:  []string{"bucket"},
			functions:  []string{"toLower"},
			expression: `toLower(bucket.name)`,
			wantResources: []ResourceDependency{
				{Name: "bucket", Path: "bucket.name"},
			},
			wantFunctions: []FunctionCall{
				{Name: "toLower"},
			},
		},
		{
			name:       "deployment replicas with function",
			resources:  []string{"deployment"},
			functions:  []string{"max"},
			expression: `max(deployment.spec.replicas, 5)`,
			wantResources: []ResourceDependency{
				{Name: "deployment", Path: "deployment.spec.replicas"},
			},
			wantFunctions: []FunctionCall{
				{Name: "max"},
			},
		},
		{
			name:       "OR and index operators simple",
			resources:  []string{"list", "flags"},
			functions:  []string{},
			expression: `list[0] || flags["enabled"]`,
			wantResources: []ResourceDependency{
				{Name: "list", Path: "list"},
				{Name: "flags", Path: "flags"},
			},
			wantFunctions: []FunctionCall{},
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
				{Name: "process"},
			},
		},
		{
			name:       "test operator string conversion",
			resources:  []string{"list", "conditions"},
			functions:  []string{"validate"},
			expression: `validate(conditions.ready || conditions.initialized && list[3])`,
			wantResources: []ResourceDependency{
				{Name: "list", Path: "list"},
				{Name: "conditions", Path: "conditions.ready"},
				{Name: "conditions", Path: "conditions.initialized"},
			},
			wantFunctions: []FunctionCall{
				{Name: "validate", Arguments: []string{
					"(conditions.ready || conditions.initialized) && list[3]",
				}},
			},
		},
		{
			name:       "eks and nodegroup check",
			resources:  []string{"eksCluster", "nodeGroup"},
			expression: `eksCluster.spec.version == nodeGroup.spec.version`,
			wantResources: []ResourceDependency{
				{Name: "eksCluster", Path: "eksCluster.spec.version"},
				{Name: "nodeGroup", Path: "nodeGroup.spec.version"},
			},
		},
		{
			name:       "deployment and cluster version",
			resources:  []string{"deployment", "eksCluster"},
			expression: `deployment.metadata.namespace == "default" && eksCluster.spec.version == "1.31"`,
			wantResources: []ResourceDependency{
				{Name: "deployment", Path: "deployment.metadata.namespace"},
				{Name: "eksCluster", Path: "eksCluster.spec.version"},
			},
		},
		{
			name:       "eks name and bucket prefix",
			resources:  []string{"eksCluster", "bucket"},
			functions:  []string{"concat", "toLower"},
			expression: `concat(toLower(eksCluster.spec.name), "-", bucket.spec.name)`,
			wantResources: []ResourceDependency{
				{Name: "eksCluster", Path: "eksCluster.spec.name"},
				{Name: "bucket", Path: "bucket.spec.name"},
			},
			wantFunctions: []FunctionCall{
				{Name: "concat"},
				{Name: "toLower"},
			},
		},
		{
			name:       "instances count",
			resources:  []string{"instances"},
			functions:  []string{"count"},
			expression: `count(instances) > 0`,
			wantResources: []ResourceDependency{
				{Name: "instances", Path: "instances"},
			},
			wantFunctions: []FunctionCall{
				{Name: "count"},
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
				{Name: "fargateProfile", Path: "fargateProfile.spec.subnets"},
				{Name: "fargateProfile", Path: "fargateProfile.spec.selectors"},
				{Name: "eksCluster", Path: "eksCluster.status.state"},
			},
			wantFunctions: []FunctionCall{
				{Name: "contains"},
				{Name: "count"},
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
				{Name: "securityGroup", Path: "securityGroup.spec.vpcID"},
				{Name: "securityGroup", Path: "securityGroup.spec.rules"},
				{Name: "vpc", Path: "vpc.status.vpcID"},
			},
			wantFunctions: []FunctionCall{
				{Name: "concat"},
				{Name: "contains"},
				{Name: "map"}, // first map is for rules
				{Name: "map"}, // second map is for ipRanges
			},
		},
		{
			name:      "eks cluster validation",
			resources: []string{"eksCluster", "nodeGroups", "iamRole", "vpc"},
			functions: []string{"filter", "contains", "timeAgo"}, // duration and size are a built-in function
			expression: `eksCluster.status.state == "ACTIVE" && 
				duration(timeAgo(eksCluster.status.createdAt)) > duration("24h") && 
				size(nodeGroups.filter(ng,
					ng.status.state == "ACTIVE" &&
					contains(ng.labels, "environment"))) >= 1 && 
				contains(map(iamRole.policies, p, p.actions), "eks:*") && 
				size(vpc.subnets.filter(s, s.isPrivate)) >= 2`,
			wantResources: []ResourceDependency{
				{Name: "eksCluster", Path: "eksCluster.status.state"},
				{Name: "eksCluster", Path: "eksCluster.status.createdAt"},
				{Name: "nodeGroups", Path: "nodeGroups"},
				{Name: "iamRole", Path: "iamRole.policies"},
				{Name: "vpc", Path: "vpc.subnets"},
			},
			wantFunctions: []FunctionCall{
				{Name: "contains"},
				{Name: "contains"},
				{Name: "map"},
				{Name: "map"},
				{Name: "timeAgo"},
				// built-in functions don't appear in the function list
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
				{Name: "order", Path: "order.total"},
				{Name: "order", Path: "order.total"},
				{Name: "order", Path: "order.items"},
				{Name: "product", Path: "product.id"},
				{Name: "inventory", Path: "inventory.stock"},
				{Name: "customer", Path: "customer.shippingAddress"},
				{Name: "customer", Path: "customer.address.zipCode"},
			},
			wantFunctions: []FunctionCall{
				{Name: "validateAddress"},
				{Name: "map"},
				{Name: "calculateTax"},
			},
		},
		{
			name:       "filter with explicit condition",
			resources:  []string{"pods"},
			functions:  []string{},
			expression: `pods.filter(p, p.status == "Running")`,
			wantResources: []ResourceDependency{
				{Name: "pods", Path: "pods"},
			},
			wantFunctions: []FunctionCall{
				{Name: "map"},
			},
		},
		{
			name:          "create message struct",
			resources:     []string{},
			functions:     []string{"createPod"},
			expression:    `createPod(Pod{metadata: {name: "test", labels: {"app": "web"}}, spec: {containers: [{name: "main", image: "nginx"}]}})`,
			wantResources: nil,
			wantFunctions: []FunctionCall{
				{Name: "createPod"},
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
				{Name: "processMap"},
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
				{Name: "validate"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inspector, err := NewInspector(tt.resources, tt.functions)
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

			sortDependencies(got.ResourceDependencies)
			sortDependencies(tt.wantResources)
			sortFunctions(got.FunctionCalls)
			sortFunctions(tt.wantFunctions)

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
				{Name: "unknownResource", Path: "unknownResource"},
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
				{Name: "unknown", Path: "unknown"},
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
				{Name: "instances", Path: "instances"},
			},
			wantFunctions: []FunctionCall{
				{Name: "map"},
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
				{Name: "i", Path: "i.status"},
				{Name: "instances", Path: "instances"},
			},
			wantFunctions: []FunctionCall{
				{Name: "map"},
			},
			wantUnknownRes: nil,
		},
		{
			name:       "test target function chaining",
			resources:  []string{"bucket"},
			functions:  []string{"processItems", "validate"},
			expression: `processItems(bucket).validate()`,
			wantResources: []ResourceDependency{
				{Name: "bucket", Path: "bucket"},
			},
			wantFunctions: []FunctionCall{
				{Name: "processItems"},
				{Name: "processItems(bucket).validate"},
			},
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
				{Name: "result", Path: "result"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inspector, err := NewInspector(tt.resources, tt.functions)
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

func Test_InvalidExpression(t *testing.T) {
	_ = NewInspectorWithEnv(nil, []string{""}, []string{""})

	inspector, err := NewInspector([]string{}, []string{})
	if err != nil {
		t.Fatalf("Failed to create inspector: %v", err)
	}
	_, err = inspector.Inspect("invalid expression ######")
	if err == nil {
		t.Errorf("Expected error")
	}
}
