#!/bin/sh
# Starts a fresh Garage (tmpfs), creates the test key and bucket and prints the S3 test variables.
#   eval "$(testenv/garage/seed.sh)"
set -e
cd "$(dirname "$0")"
KEY=GK0123456789abcdef01234567
SECRET=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
g() { docker compose exec -T garage /garage "$@"; }
docker compose up -d --force-recreate >&2
i=0
until g status >/dev/null 2>&1; do
  i=$((i+1)); [ $i -gt 60 ] && { echo "garage did not come up" >&2; docker compose logs --tail 30 >&2; exit 1; }
  sleep 1
done
NODE=$(g node id -q 2>/dev/null | cut -d@ -f1)
g layout assign -z test -c 1G "$NODE" >&2
g layout apply --version 1 >&2
g key import --yes -n test "$KEY" "$SECRET" >&2
g bucket create odraz-test >&2
g bucket allow --read --write --owner odraz-test --key "$KEY" >&2
echo "export ODRAZ_TEST_S3_ENDPOINT=http://localhost:9302"
echo "export ODRAZ_TEST_S3_BUCKET=odraz-test"
echo "export ODRAZ_TEST_S3_KEY=$KEY"
echo "export ODRAZ_TEST_S3_SECRET=$SECRET"
