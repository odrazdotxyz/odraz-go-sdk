#!/bin/sh
# Starts Nextcloud (first start installs it: about a minute), installs the Team folders app and creates:
#   users alice / alice-pass-123 (group "team": full access) and bob / bob-pass-123 (group "viewers": read only)
#   a team folder "Team Docs" with a 64 MiB quota.
# Safe to run again; state lives in tmpfs, so a restart wipes it.
set -e
cd "$(dirname "$0")"
docker compose up -d
for i in $(seq 120); do
  [ "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:8082/status.php)" = 200 ] && break
  sleep 3
done
occ() { docker compose exec -T -u www-data nextcloud php occ "$@"; }
occ app:install groupfolders >/dev/null 2>&1 || occ app:enable groupfolders >/dev/null
occ group:add team >/dev/null 2>&1 || true
occ group:add viewers >/dev/null 2>&1 || true
docker compose exec -T -u www-data -e OC_PASS=alice-pass-123 nextcloud php occ user:add --password-from-env --group=team alice >/dev/null 2>&1 || true
docker compose exec -T -u www-data -e OC_PASS=bob-pass-123 nextcloud php occ user:add --password-from-env --group=viewers bob >/dev/null 2>&1 || true
if ! occ groupfolders:list --output=json | grep -q '"Team Docs"'; then
  id=$(occ groupfolders:create "Team Docs" | tail -1)
  occ groupfolders:group "$id" team read write share delete >/dev/null
  occ groupfolders:group "$id" viewers read >/dev/null
  occ groupfolders:quota "$id" 64MB >/dev/null
fi
echo "export ODRAZ_TEST_NEXTCLOUD_URL=http://localhost:8082"
