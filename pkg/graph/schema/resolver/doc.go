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

// Package resolver provides OpenAPI schema resolution for kro's graph builder.
//
// The package provides three resolver implementations and a ChainedResolver to
// compose them. The recommended resolution order is:
//
//   - Core (DefaultCoreResolver): compiled-in OpenAPI definitions for built-in
//     Kubernetes types. Always available, zero cost.
//
//   - CRD (DefaultCRDResolver): event-driven informer for CRD schemas. Lazy
//     extraction — only CRDs that kro references pay the parsing cost. The
//     caller must register it with the controller manager (mgr.Add).
//
//   - Fallback (DefaultFallbackResolver): TTL-cached discovery client for
//     everything else — aggregated API types (metrics-server, custom API
//     servers) and a warm-up fallback while the CRD informer cache is cold.
//
// The CRD informer starts asynchronously via the controller manager. On the
// first ResolveSchema call the informer cache may not yet be synced; misses
// fall through to the fallback resolver by design.
package resolver
