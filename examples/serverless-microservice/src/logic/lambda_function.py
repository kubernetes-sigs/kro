# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: Apache-2.0

# Implementation of the API backend for a microservice
import json
import uuid
import os
import boto3
from datetime import datetime

# Prepare DynamoDB client
TABLE_NAME = os.getenv('TABLE_NAME', None)
dynamodb = boto3.resource('dynamodb')
ddbTable = dynamodb.Table(TABLE_NAME)


def lambda_handler(event, context):
    print(event)
    route_key = f"{event['requestContext']['http']['method']} {event['requestContext']['http']['path']}"

    # Set default response, override with data from DynamoDB if any
    response_body = {'Message': 'Unsupported route'}
    status_code = 400
    headers = {
        'Content-Type': 'application/json',
        'Access-Control-Allow-Origin': '*'
        }

    try:
        # Get all records
        if route_key == 'GET /items':
            ddb_response = ddbTable.scan(Select='ALL_ATTRIBUTES')
            # return list of items instead of full DynamoDB response
            response_body = ddb_response['Items']
            status_code = 200
        # Other CRUD operations
        if route_key == 'GET /items/{id}':
            # get data from the database
            ddb_response = ddbTable.get_item(
                Key={'id': event['pathParameters']['id']}
            )
            # return list of items instead of full DynamoDB response
            if 'Item' in ddb_response:
                response_body = ddb_response['Item']
            else:
                response_body = {}
            status_code = 200
        if route_key == 'DELETE /items/{id}':
            # delete item in the database
            ddbTable.delete_item(
                Key={'id': event['pathParameters']['id']}
            )
            response_body = {}
            status_code = 200
        if route_key == 'PUT /items':
            request_json = json.loads(event['body'])
            request_json['timestamp'] = datetime.now().isoformat()
            # generate unique id if it isn't present in the request
            if 'id' not in request_json:
                request_json['id'] = str(uuid.uuid1())
            # update the database
            ddbTable.put_item(
                Item=request_json
            )
            response_body = request_json
            status_code = 200
    except Exception as err:
        status_code = 400
        response_body = {'Error:': str(err)}
        print(str(err))
    return {
        'statusCode': status_code,
        'body': json.dumps(response_body),
        'headers': headers
    }