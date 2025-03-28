# SSL-enabled Ingress

This example demonstrates how to use KRO  to orchestrate complex resource relationships, creating a fully automated SSL-enabled ingress setup with certificate management and DNS configuration.

## Architecture Overview

The example uses a two-layer RGD approach:

1. **IngressTriangle RGD**: Base layer managing SSL/DNS infrastructure
   - ACM Certificate management
   - Route53 DNS validation
   - ALB Ingress configuration

2. **ServiceIngress RGD**: Application layer composing:
   - IngressTriangle instance
   - Kubernetes Deployment
   - Kubernetes Service

## Prerequisites

### Required Controllers


1. **AWS Controllers for Kubernetes (ACK)**
   - **ACM Controller**
     - Manages AWS Certificate Manager resources
     - Handles SSL/TLS certificate lifecycle
     - Required for certificate validation and management

   - **Route53 Controller**
     - Manages AWS Route53 DNS records
     - Handles DNS validation records
     - Required for domain validation and DNS management

2. **AWS Load Balancer Controller**
   - Manages AWS Application Load Balancer (ALB) resources
   - Required for Ingress resource implementation
   - Handles SSL termination and routing

## RGDs

### 1. IngressTriangle RGD - Base Infrastructure

The IngressTriangle RGD shows how KRO manages resource relationships and dependencies:

```yaml
# Status fields show how we track and expose resource states to use to other RGDs
status:
  validationStatus: ${certificateResource.status.domainValidations[0].validationStatus}
  certificateARN:  ${certificateResource.status.ackResourceMetadata.arn}
  loadBalancerARN: ${ingress.status.loadBalancer.ingress[0].hostname}
```


Example of resource dependency in IngressTriangle:
```yaml
# Ingress waits for certificate validation before using the ARN
annotations:
  alb.ingress.kubernetes.io/certificate-arn: '${certificateResource.status.domainValidations[0].validationStatus == "SUCCESS" ? 
    certificateResource.status.ackResourceMetadata.arn : null}'
```

### 2. ServiceIngress RGD - Resources

ServiceIngress demonstrates how to use multipel RGDs:

```yaml
resources:
  - id: ingresstriangle
    readyWhen:
      - ${ingresstriangle.status.loadBalancerARN != null}
    template:
      apiVersion: kro.run/v1alpha1
      kind: IngressTriangle    # Using IngressTriangle RGD as a resource
      spec:
        name: ${schema.spec.name}
        subDomain: ${schema.spec.subDomain}
```

Key concepts:
1. **RGD**: Use IngressTriangle as a resource within ServiceIngress
2. **Status Usage**: Access IngressTriangle's exposed status values
3. **Readiness Conditions**: Wait for specific conditions before proceeding
4. **Parameter Passing**: Pass values from ServiceIngress to IngressTriangle

Example of consuming IngressTriangle status:
```yaml
# ServiceIngress waits for LoadBalancer before proceeding
readyWhen:
  - ${ingresstriangle.status.loadBalancerARN != null}
```

This orchestration allows:
- Reuse of complex infrastructure setups
- Hierarchical resource management
- Clean separation of concerns between infrastructure and application resources
```
