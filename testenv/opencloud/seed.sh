#!/bin/sh
# Starts a fresh OpenCloud and prints an app token for the admin user.
#   eval "$(testenv/opencloud/seed.sh)"   # exports ODRAZ_TEST_OPENCLOUD_URL/USER/TOKEN
set -e
cd "$(dirname "$0")"
docker compose up -d --force-recreate >&2
i=0
until curl -skf -m 5 -o /dev/null https://localhost:9200/graph/v1.0/me -u admin:admin; do
  i=$((i+1)); [ $i -gt 60 ] && { echo "opencloud did not come up" >&2; docker compose logs --tail 30 >&2; exit 1; }
  sleep 2
done
TOKEN=$(docker compose exec -T opencloud opencloud auth-app create --user-name=admin --expiration=720h 2>&1 | sed -n 's/^ *token: *//p')
[ -n "$TOKEN" ] || { echo "could not create app token" >&2; exit 1; }
echo "export ODRAZ_TEST_OPENCLOUD_URL=https://localhost:9200"
echo "export ODRAZ_TEST_OPENCLOUD_USER=admin"
echo "export ODRAZ_TEST_OPENCLOUD_PASSWORD=admin"
echo "export ODRAZ_TEST_OPENCLOUD_OIDC_CLIENT=odraz-test   # registered in testenv/opencloud/idp.yaml"
echo "export ODRAZ_TEST_OPENCLOUD_TOKEN='$TOKEN'"
