# Steps to deploy ack-controllers to cluster

## Deploying Controllers
### Prerequisites
Create IRSA for IAM controller
See [ACK Docs] (https://aws-controllers-k8s.github.io/community/docs/user-docs/irsa/)

### Deployment order:
1. IAM
2. EC2
3. EKS

### Steps
For these EKS and EC2 controllers we are using the IAM controller to create
the necessary roles for the service account
1. Deploy Controller CRD Group
2. Deploy Controller ResourceGroup
3. Deploy Controller Instance (don't forget to include required fields) 