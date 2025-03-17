# Description

This PR adds support for GitOps applications using KRO and Flux CD. The implementation allows users to define and manage GitOps applications through a custom resource definition (CRD) that orchestrates Deployments, Services, and Flux CD Kustomizations.

## Changes Made

1. Added `GitOpsApp` ResourceGraphDefinition with:
   - Schema validation for app configuration
   - Support for environment-based deployments (dev/prod)
   - Integration with Flux CD for GitOps workflows

2. Fixed schema format issues:
   - Simplified schema definition to use basic types (string, integer)
   - Removed complex OpenAPI v3 validation to ensure compatibility
   - Fixed issues with enum and default value handling

## Test Results

1. ResourceGraphDefinition:
   - Successfully created and activated
   - Schema validation working correctly
   - Graph verification passing

2. GitOpsApp Instance:
   - Created successfully with test configuration
   - Deployment and Service resources created and running
   - Successfully updated configuration (replicas: 1 -> 3, image: nginx:latest -> nginx:1.25)
   - Changes propagated correctly to child resources
   - Kustomization pending GitRepository setup (expected behavior)

3. Controller Behavior:
   - Properly reconciles resource changes
   - Handles updates to replicas and image versions
   - Maintains desired state across updates
   - Logs show correct delta detection and resource updates

## Next Steps

1. Set up GitRepository for Flux CD integration
2. Add documentation for GitOps workflow
3. Add examples for different deployment patterns

## Checklist

- [x] Code builds successfully
- [x] Tests pass
- [x] Documentation updated
- [x] CRD schema validated
- [x] Resource reconciliation tested
- [ ] GitOps workflow documented 