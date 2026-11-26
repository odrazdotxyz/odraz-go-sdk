#!/bin/sh
# Starts a fresh SeaweedFS (tmpfs), creates the test bucket and prints the S3 test variables.
#   eval "$(testenv/seaweedfs/seed.sh)"
set -e
cd "$(dirname "$0")"
docker compose up -d --force-recreate >&2
go run ../minio/mkbucket localhost:9301 odraz-test-only odraz-test-only-not-a-secret odraz-test >&2 || { docker compose logs --tail 30 >&2; exit 1; }
echo "export ODRAZ_TEST_S3_ENDPOINT=http://localhost:9301"
echo "export ODRAZ_TEST_S3_BUCKET=odraz-test"
echo "export ODRAZ_TEST_S3_KEY=odraz-test-only"
echo "export ODRAZ_TEST_S3_SECRET=odraz-test-only-not-a-secret"
