# Example serverless architecture - microservice


This REST API example shows the end-to-end implementation of a simple application using a serverless approach, as depicted in the diagram:

![Serverless microservice diagram](./assets/architecture.png)

Example is (loosely) based on a AWS Serverless Samples repository [serverless-rest-api](https://github.com/aws-samples/serverless-samples/tree/main/serverless-rest-api) project. The services used by this application include Amazon API Gateway, AWS Lambda, and Amazon DynamoDB. Observability implementation is based on Amazon CloudWatch Dashboards and MetricAlarms. This example skips CI/CD implementation and unit/integration testing.

## Implementation notes

### API
API uses API Gateway HTTP API endpoint type. Requests are passed to the integration target (AWS Lambda) for routing and interpretation/response generation. API Gateway does not implement any validation, transformation, path-based routing, API management functions. 


API Gateway uses Lambda Authorizer for authentication/authorization. However, sample implementation at `./src/authorizer/lambda_function.py` allows all actions on all resources in the API if the  `Authorization` header value in the request matches the one stored in the AWS Secrets Manager and retrieved by the Lambda Authorizer when it initializes. 

Make sure to update the authorizer Lambda code according to your authentication/authorization needs. For more details on how to implement Lambda Authorizer, check out [documentation](https://docs.aws.amazon.com/apigateway/latest/developerguide/apigateway-use-lambda-authorizer.html). or [blueprints](https://github.com/awslabs/aws-apigateway-lambda-authorizer-blueprints). 
Look at Lambda Authorizer code at [serverless-rest-api](https://github.com/aws-samples/serverless-samples/tree/main/serverless-rest-api) for JWT based authorization examples if needed.


### Business logic
API Gateway passes the incoming requests to the Lambda function and returns response to the API client. Sample implementation code is available at `./src/logic/lambda_function.py`. It expects the database table name to be specified in the environment variable `TABLE_NAME`. 

For HTTP GET requests to the API `items` resource, it runs Amazon DynamoDB `scan` operation and returns all items received as a result. For HTTP GET requests for a particular item (the `items\{id}` resource) it performs `get_item` operation and returns a response from the DynamoDB. PUT request to `items` resource takes incoming payload, adds UUID as a hash key value, adds current timestamp, and performs DynamoDB `put_item` operation. It returns the payload sent to the Dynamo DB as a response body to the API client.

### Database
Example uses DynamoDB table to store data. Database definition is hardcoded in the composition and includes a single required `id` field that is used as a hash key. You will need to change this structure and business logic Lambda code to implement anything more complicated than simple CRUD operations.

# Deployment


## Pre-requisites:
- EKS cluster
- [Kubectl](https://kubernetes.io/docs/tasks/tools/)
- [AWS ACK](https://aws-controllers-k8s.github.io/community/docs/community/overview/)

Check out `./src/install.sh` script for the commands used to install necessary ACK controllers .

### Deploy ResourceGroup

Make sure you are in the following directory:
```shell
cd examples/serverless-microservice/
```

```shell
kubectl apply -f microservice.yaml
```

Verify the ResourceGroups
```shell
kubectl get ResourceGroup
```

Expected output
```
NAME                              AGE
microservice.x.symphony.k8s.aws   14m
```

### Build Lambda function packages
Make sure you are in the following directory:
```shell
cd examples/serverless-microservice/
```

Set the AWS region and S3 bucket name to be used by the Lambda build/package process and in the claim:

```shell
export AWS_REGION=<replace-with-aws-region> # example `us-east-1`
export S3_BUCKET=<replace-with-s3-bucket-name> # example `my-serverless-microservice-lambdas`
```

Executing the `build-and-upload-zip.sh` script creates an S3 bucket in a specified region, zips the Lambda functions, and uploads the ZIP file to the S3 bucket. If the bucket already exists and you have access to it, the script will print a message and continue with the upload. 
```shell
./build-and-upload-zip.sh --bucket $S3_BUCKET --region $AWS_REGION
```

### Update and apply the claim

Change the default value for `CLAIM_NAME` with any name you choose.
```shell
export CLAIM_NAME=<replace-with-claim-name> # example `test-rest-api`
```

Run the below commands to generate a random password to be used by a Lambda Authorizer and store it in [AWS Secrets Manager](https://aws.amazon.com/secrets-manager/)
```shell
export AUTHORIZER_PASSWORD=$(aws secretsmanager get-random-password --output text)
export SECRET_ARN=$(aws secretsmanager create-secret --name "$CLAIM_NAME-auth-password" --secret-string "$AUTHORIZER_PASSWORD" --output json | jq .ARN | tr -d '"')
```

*Note that password is stored in the AUTHORIZER_PASSWORD environment variable, also used by the testing scripts later in this document. If needed, you can retrieve a password from the AWS Secrets Manager using following command:*
```shell
 aws secretsmanager get-secret-value --secret-id "$CLAIM_NAME-auth-password"     
```

Run the below command to use the template file `microservice-claim-tmpl.yaml` to create the claim file with the variables `CLAIM_NAME`, `S3_BUCKET`, `SECRET_ARN`, and `AWS_REGION` substituted.
```shell
envsubst < "microservice-claim-tmpl.yaml" > "claim.yaml"
```


Check that the claim populated with values. Update prefix, API name or description values in the claim if desired.
```
cat claim.yaml
```

Apply the claim
```shell
kubectl apply -f claim.yaml
```

Validate the claim
```
kubectl get microservice
```

Expected result
```
NAME                AGE
test-microservice   12m
```

## Troubleshooting

Get a list of the resource groups
```
kubectl get ResourceGroup
```
Expected result 
```
NAME                              AGE
microservice.x.symphony.k8s.aws   35m
```

Describe your resource group, look for errors and events:
```
kubectl describe resourcegroup.x.symphony.k8s.aws/microservice.x.symphony.k8s.aws
```
Expected result (resource definitions removed for brevity)
```
Name:         microservice.x.symphony.k8s.aws
Namespace:    default
Labels:       <none>
Annotations:  <none>
API Version:  x.symphony.k8s.aws/v1alpha1
Kind:         ResourceGroup
Metadata:
  Creation Timestamp:  2024-07-11T15:34:53Z
  Finalizers:
    symphony.io/finalizer
  Generation:        2
  Resource Version:  462737
  UID:               63729ad1-056c-4c33-a966-3ad8e798ec32
Spec:
  API Version:  v1alpha1
  Definition:
    Spec:
      API:
        API Description:  string | default="Microservice that uses Amazon API Gateway and AWS Lambda"
        API Name:         string | default="my-api"
      Lambda:
        Code Bucket Name:       string | required=true
        Logic Lambda Code:      string | default="microservice-business-logic.zip"
        Logic Lambda Handler:   string | default="lambda_function.lambda_handler"
        Logic Lambda Run Time:  string | default="python3.10"
      Name Prefix:              string | default="demo"
      Region:                   string | required=true
  Kind:                         Microservice
  Resources:
    Definition:
<... resources' definitions removed for brevity ...>
Status:
  Conditions:
    Last Transition Time:  2024-07-11T15:36:38Z
    Message:               micro controller is ready
    Reason:                
    Status:                True
    Type:                  symphony.aws.dev/ReconcilerReady
    Last Transition Time:  2024-07-11T15:36:38Z
    Message:               Directed Acyclic Graph is synced
    Reason:                
    Status:                True
    Type:                  symphony.aws.dev/GraphVerified
    Last Transition Time:  2024-07-11T15:36:38Z
    Message:               Custom Resource Definition is synced
    Reason:                
    Status:                True
    Type:                  symphony.aws.dev/CustomResourceDefinitionSynced
  State:                   ACTIVE
  Topological Order:
    itemsTable
    lambdaBasicPolicy
    apigw
    apigwItemsIdRoute
    apigwItemsRoute
    apigwDefaultStage
    lambdaDDBAccessPolicy
    logicLambdaRole
    logicLambda
    apigwLambdaIntegration
Events:  <none>
```

Check logs of the Symphony pod for errors if necessary (this command assumes there is only one Symphony pod available):
```
kubectl get pods -o custom-columns=":metadata.name" | grep symphony | xargs -I% kubectl logs "%" --since=1h
```

Check logs of the ACK controller pod for errors if necessary (this command assumes there is only one Lambda controller pod available):
```
kubectl get pods -o custom-columns=":metadata.name" | grep "ack-lambda-controller" | xargs -I% kubectl logs "%" --since=1h
```


Describe your microservice:
```
kubectl describe microservice.x.symphony.k8s.aws/test-microservice
```

Check the state of individual resources if needed, look for the errors and events:
```
kubectl describe Function
```

Check for errors, events (in this case function IAM role is missing):
```
Name:         test-logic
Namespace:    default
Labels:       <none>
Annotations:  <none>
API Version:  lambda.services.k8s.aws/v1alpha1
Kind:         Function
Metadata:
  Creation Timestamp:  2024-07-10T16:52:37Z
  Generation:          1
  Resource Version:    227469
  UID:                 a4bd7a47-ad5d-4406-a92f-089a6d92dbc2
Spec:
  Code:
    s3Bucket:  gpk-tests-or
    s3Key:     microservice-business-logic.zip
  Environment:
    Variables:
  Handler:       lambda_function.lambda_handler
  Name:          test-logic
  Package Type:  zip
  Runtime:       python3.10
Status:
  Conditions:
    Last Transition Time:  2024-07-10T16:55:21Z
    Message:               Reference resolution failed
    Reason:                resource reference wrapper or ID required: Role,RoleRef
    Status:                Unknown
    Type:                  ACK.ReferencesResolved
Events:                    <none>
```

## Clean Up
Delete the serverless application
```shell
kubectl delete -f claim.yaml
```

Delete the S3 bucket you've created and the Lambda ZIP packages in it


Delete the ResourceGroups
```shell
kubectl delete -f microservice.yaml
```

*Note:*
*In case deletion process hangs, you may try patching microservice finalizer:*
```
kubectl patch Microservice/test-microservice -p '{"metadata":{"finalizers":[]}}' --type=merge
```