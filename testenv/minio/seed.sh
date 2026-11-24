#!/bin/sh
# Starts a fresh MinIO (tmpfs), creates the test bucket and prints the S3 test variables.
#   eval "$(testenv/minio/seed.sh)"   # exports ODRAZ_TEST_S3_ENDPOINT/BUCKET/KEY/SECRET
# Credentials are demo values for this throwaway container only.
set -e
cd "$(dirname "$0")"
docker compose up -d --force-recreate >&2
go run ./mkbucket localhost:9300 odraz-test-only odraz-test-only-not-a-secret odraz-test >&2 || { docker compose logs --tail 30 >&2; exit 1; }
echo "export ODRAZ_TEST_S3_ENDPOINT=http://localhost:9300"
echo "export ODRAZ_TEST_S3_BUCKET=odraz-test"
echo "export ODRAZ_TEST_S3_KEY=odraz-test-only"
echo "export ODRAZ_TEST_S3_SECRET=odraz-test-only-not-a-secret"
