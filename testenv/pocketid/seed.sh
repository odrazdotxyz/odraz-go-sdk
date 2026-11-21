#!/bin/sh
# Starts a fresh Pocket ID and prints the variables the tests need.
#   eval "$(testenv/pocketid/seed.sh)"
set -e
cd "$(dirname "$0")"
docker compose up -d --force-recreate >&2
i=0
until curl -sf -m 3 -o /dev/null http://localhost:1411/.well-known/openid-configuration; do
  i=$((i+1)); [ $i -gt 40 ] && { echo "pocket id did not come up" >&2; exit 1; }
  sleep 1
done
echo "export ODRAZ_TEST_POCKETID_URL=http://localhost:1411"
echo "export ODRAZ_TEST_POCKETID_API_KEY=odraz-test-static-api-key-0123456789"
