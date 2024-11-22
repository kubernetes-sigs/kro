import sys
import boto3
import json
from datetime import datetime
from pyspark.sql import SparkSession
from pyspark.sql.functions import *
from pyspark.sql import functions as f

appName = "STACK_NAME"+datetime.now().strftime("%Y_%m_%d_%H_%M_%S")

def map_payment_type(payment_type):
    payment_map = {
        1: "Credit card",
        2: "Cash",
        3: "No charge",
        4: "Dispute",
        5: "Unknown",
        6: "Voided trip"
    }
    return payment_map.get(payment_type, "Other")

def main(args):
    result={}
    input_bucket_name = args[1]
    input_file_name = args[2]
    output_bucket_name = args[3]
    output_file_name = args[4]

    spark = SparkSession \
        .builder \
        .appName(appName) \
        .getOrCreate()

    # Load data from S3
    df = (spark.read.format("parquet") \
            .option("header", "true") \
            .option("inferSchema", "true") \
            .load("s3://"+input_bucket_name+"/"+input_file_name)) \
            .dropna()

    # Get data summary
    df_summary = df.groupBy("VendorID").agg(
        avg("Fare_amount").alias("avg_fare"), 
        count("*").alias("num_trips"),
        sum("Fare_amount").alias("total_fare")
    )
    
    # Get most expensive trips 
    top_10_expensive_trips = df.orderBy(f.col("fare_amount").desc()).limit(10)

    # Create UDF for payment type mapping
    map_payment_type_udf = f.udf(map_payment_type, StringType())
    # Calculate distribution by payment_type
    payment_type_distribution = df.groupBy("payment_type") \
        .agg(f.count("*").alias("trip_count"), 
             f.round(f.avg("fare_amount"), 2).alias("avg_fare"),
             f.round(f.sum("fare_amount"), 2).alias("total_fare")) \
        .withColumn("payment_type_desc", map_payment_type_udf(f.col("payment_type"))) \
        .orderBy(f.col("trip_count").desc())    
    # Calculate percentage distribution
    total_trips = df.count()
    payment_type_percentage = payment_type_distribution.withColumn(
        "percentage", f.round((f.col("trip_count") / total_trips) * 100, 2)
    )    

    # Generate result
    result = {
        "data_file": input_file_name.split("/")[-1], 
        "source_url": "s3://"+input_bucket_name+input_file_name,
        "data_summary": [json.loads(x) for x in df_summary.toJSON().collect()],
        "top_10_most_expensive_trips": [json.loads(x) for x in top_10_expensive_trips.toJSON().collect()],
        "payment_types": [json.loads(x) for x in payment_type_percentage.toJSON().collect()]
        }
        
    # Write result to S3
    s3 = boto3.resource('s3')
    object = s3.Object(output_bucket_name, output_file_name)
    object.put(Body=json.dumps(result))
    
    spark.stop()

    return None
    
    
if __name__ == "__main__":
    print(len(sys.argv))
    if len(sys.argv) != 5:
        print("Usage: process_data input_bucket_name input_file_name output_bucket_name output_file_name")
        sys.exit(0)

    main(sys.argv)
    
