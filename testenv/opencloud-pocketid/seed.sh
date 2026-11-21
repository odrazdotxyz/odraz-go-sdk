#!/bin/sh
# Fresh OpenCloud + Pocket ID (OpenCloud trusts Pocket ID as its OIDC provider).
# Stops testenv/opencloud and testenv/pocketid first: same ports (9200, 1411).
#   eval "$(testenv/opencloud-pocketid/seed.sh)"
set -e
cd "$(dirname "$0")"
(cd ../opencloud && docker compose down >/dev/null 2>&1) || true
(cd ../pocketid && docker compose down >/dev/null 2>&1) || true
docker compose up -d --force-recreate >&2
i=0
until curl -sf -m 3 -o /dev/null http://localhost:1411/.well-known/openid-configuration \
   && curl -skf -m 5 -o /dev/null "https://localhost:9200/.well-known/webfinger?resource=https://localhost:9200&rel=http://openid.net/specs/connect/1.0/issuer"; do
  i=$((i+1)); [ $i -gt 60 ] && { echo "did not come up" >&2; docker compose logs --tail 20 >&2; exit 1; }
  sleep 2
done
echo "export ODRAZ_TEST_OCPOCKET_URL=https://localhost:9200"
echo "export ODRAZ_TEST_POCKETID_URL=http://localhost:1411"
echo "export ODRAZ_TEST_POCKETID_API_KEY=odraz-test-static-api-key-0123456789"
