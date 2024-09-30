#!/bin/bash

set -e
set -o pipefail

if [ -z "$S3_BUCKET" ] && [ -z "$1" ]
then
  echo "Error: S3 bucket name not specified"
  echo "Usage: $0 [-b|--bucket S3_BUCKET] [-r|--region AWS_REGION]"
  exit 1
fi

if [ -n "$1" ]
then
  S3_BUCKET="$1"
fi

while [ $# -gt 0 ]
do
  case "$1" in
    -b|--bucket)
      S3_BUCKET="$2"
      shift
      shift
      ;;
    -r|--region)
      REGION="$2"
      shift
      shift
      ;;
    *)
      echo "Error: Unrecognized option: $1"
      echo "Usage: $0 [-b|--bucket S3_BUCKET] [-r|--region AWS_REGION]"
      exit 1
      ;;
  esac
done

if [ -z "$REGION" ] && [ -z "$2" ]
then
  echo "Error: AWS region not specified"
  echo "Usage: $0 [-b|--bucket S3_BUCKET] [-r|--region AWS_REGION]"
  exit 1
fi

if [ -n "$2" ]
then
  REGION="$2"
fi

if ! aws s3api head-bucket --bucket "$S3_BUCKET" >/dev/null 2>&1
then
  echo "Creating S3 bucket $S3_BUCKET in region $REGION"
  aws s3 mb "s3://$S3_BUCKET" --region "$REGION"
else
  echo "S3 bucket $S3_BUCKET already exists"
fi

echo "Zipping functions"
cd ./src/logic/
zip microservice-business-logic.zip lambda_function.py
cd ../authorizer/
zip microservice-authorizer.zip lambda_function.py
cd ../../
echo "Uploading function to S3 bucket $S3_BUCKET"
aws s3 cp ./src/logic/microservice-business-logic.zip "s3://$S3_BUCKET/"
aws s3 cp ./src/authorizer/microservice-authorizer.zip "s3://$S3_BUCKET/"
echo "Cleaning up"
rm -f ./src/logic/microservice-business-logic.zip
rm -f ./src/authorizer/microservice-authorizer.zip