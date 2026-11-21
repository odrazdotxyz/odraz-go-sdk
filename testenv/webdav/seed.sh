#!/bin/sh
# Resets the WebDAV test server to an empty state.
set -e
cd "$(dirname "$0")"
docker compose up -d --build --force-recreate
for i in $(seq 30); do
  curl -s -o /dev/null -u odraz:odraz-test -X PROPFIND -H 'Depth: 0' http://localhost:8081/dav/ && exit 0
  sleep 1
done
echo "server did not come up" >&2; exit 1
