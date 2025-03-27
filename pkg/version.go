// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package pkg

// These variables are populated by the go compiler using -ldflags
var (
	// Version is the version of the kro binary.
	Version string = "unknown" // -X github.com/your/repo/pkg.Version=$(VERSION)
	// GitCommit is the git commit that was compiled.
	GitCommit string = "unknown" // -X github.com/your/repo/pkg.GitCommit=$(GIT_COMMIT)
	// BuildDate is the date that the kro binary was built.
	BuildDate string = "unknown" // -X github.com/your/repo/pkg.BuildDate=$(BUILD_DATE)
)
