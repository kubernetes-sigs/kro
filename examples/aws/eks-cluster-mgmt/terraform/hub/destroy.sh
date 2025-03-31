#!/bin/bash

#if var not exit provide default
TF_VAR_FILE=${TF_VAR_FILE:-"terraform.tfvars"}

terraform init
terraform destroy -var-file=$TF_VAR_FILE