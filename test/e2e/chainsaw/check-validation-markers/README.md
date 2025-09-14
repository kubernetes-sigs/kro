# Validation Markers Chainsaw Test

This test validates the new validation markers: `pattern`, `minLength`, `maxLength`, `uniqueItems`, `minItems`, and `maxItems`.

## Test Structure

1. **install-rgd**: Creates an RGD with validation markers and verifies:
   - RGD is created and ready
   - Instance CRD is generated with proper validation constraints

2. **create-valid-instance**: Creates an instance with valid data and verifies:
   - Instance is created successfully
   - ConfigMap resource is created correctly

## Validation Markers Tested

### String Validation
- **pattern**: Email regex validation `^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`
- **minLength/maxLength**: Username must be 3-15 characters
- **maxLength**: Description limited to 100 characters

### Array Validation
- **uniqueItems**: Tags array must have unique elements
- **minItems**: Tags array must have at least 1 element
- **maxItems**: Tags array can have at most 5 elements
- **minItems/maxItems**: Priorities array can have 0-3 elements

### Numeric Validation (existing)

- **minimum/maximum**: Replicas must be 1-10

## Files

- `rgd.yaml`: ResourceGraphDefinition with validation markers
- `rgd-assert.yaml`: RGD readiness assertion
- `instancecrd-assert.yaml`: CRD validation schema assertion  
- `valid-instance.yaml`: Valid instance example
- `valid-instance-assert.yaml`: Instance readiness assertion
- `configmap-assert.yaml`: Generated ConfigMap assertion

## Expected OpenAPI Schema Validation

The generated CRD should include:

- `pattern` constraints on string fields
- `minLength`/`maxLength` constraints
- `uniqueItems: true` on array fields  
- `minItems`/`maxItems` constraints on array fields
- Required field enforcement
