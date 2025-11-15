#!/bin/bash

# Clean Votes Script for Dogs vs Cats Voting App Demo
# This script clears all votes from Redis and PostgreSQL

set -e

echo "🧹 Cleaning votes for demo..."

# Get Redis and PostgreSQL endpoints
REDIS_ENDPOINT=$(kubectl get dogsvscatsapp dogsvscats-voting-app -o jsonpath='{.status.redisEndpoint}')
POSTGRES_ENDPOINT=$(kubectl get dogsvscatsapp dogsvscats-voting-app -o jsonpath='{.status.postgresEndpoint}')

echo "📍 Redis endpoint: $REDIS_ENDPOINT"
echo "📍 PostgreSQL endpoint: $POSTGRES_ENDPOINT"

# Clear Redis votes (using TLS for serverless ElastiCache)
echo "🔴 Clearing Redis votes..."
kubectl run redis-cleaner --image=redis:7-alpine --rm -it --restart=Never -- \
  redis-cli -h "$REDIS_ENDPOINT" -p 6379 --tls --insecure FLUSHALL

# Clear PostgreSQL results
echo "🐘 Clearing PostgreSQL votes..."
kubectl run postgres-cleaner --image=postgres:16 --rm -it --restart=Never \
  --env="PGPASSWORD=postgres" -- \
  psql -h "$POSTGRES_ENDPOINT" -U postgres -d postgres -c "DELETE FROM votes;"

echo "✅ All votes cleared! Ready for demo."