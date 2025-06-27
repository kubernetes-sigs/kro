# Steps to deploy ack-controllers to cluster

## Deploying Controllers
Combined ResourseGroup for ACK Controllers
- IAM
- EC2
- EKS
- ECR
- ECR Public
- SQS
- S3


### Steps
The controllers are using the IAM controller to create the necessary roles for the service account
1. Setup IRSA (IAM Roles for Service Accounts) for the IAM controller:
   See [ACK Docs] (https://aws-controllers-k8s.github.io/community/docs/user-docs/irsa/)
   
   Run the `setup_iam_controller.sh` script to create the necessary IAM role and service account. 
   `chmod +x setup_iam_controller.sh`

   ```bash
   ./setup_iam_controller.sh
   ```

   You can customize the script execution with the following options:
   
   ```
   Usage: ./setup_iam_controller.sh [OPTIONS]
   Options:
     --cluster-name NAME       Set EKS cluster name (default: curious-folk-badger)
     --region REGION           Set AWS region (default: us-west-2)
     --namespace NAMESPACE     Set Kubernetes namespace (default: kro)
     --service-account NAME    Set service account name (default: ack-iam-controller-sa)
     --help                    Display this help message
   ```

   For example, to use a different cluster name and region:

   ```bash
   ./setup_iam_controller.sh --cluster-name my-cluster --region us-east-1
   ```

   You can also set these values using environment variables:

   ```bash
   EKS_CLUSTER_NAME=my-cluster AWS_REGION=us-east-1 ./setup_iam_controller.sh
   ```

2. Install the IAM controller:
   - Apply the CRD (Custom Resource Definition)
   - Deploy the controller
   - Create an instance of the controller

3. Install all the Resource Graph Definition CRDs:
   ```
   kubectl apply -f crds/
   ```

4. Install all the controllers:
   ```
   kubectl apply -f controllers/
   ```

5. Install the combined Resource Graph Definition controllers:
   ```
   kubectl apply -f resourcegraphdefinition.yaml
   ```
6. Install the combined instance:

   All controllers are enable by default. 
   
   ```
   kubectl apply -f instance.yaml
   ```
