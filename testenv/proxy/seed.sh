#!/bin/sh
# Starts the proxy + WebDAV backend, empty.
set -e
cd "$(dirname "$0")"
docker compose up -d --build --force-recreate >&2
for i in $(seq 40); do
  curl -sf -o /dev/null -u odraz:odraz-test -H 'X-Proxy-Token: secret' -X PROPFIND -H 'Depth: 0' http://localhost:8082/dav/ && break
  [ "$i" = 40 ] && { echo "proxy did not come up" >&2; exit 1; }
  sleep 1
done
echo "export ODRAZ_TEST_PROXY_URL=http://localhost:8082/dav"
echo "export ODRAZ_TEST_PROXY_SAMEHOST_URL=http://localhost:8085/dav"
echo "export ODRAZ_TEST_PROXY_HTML_URL=http://localhost:8086/dav"
