#!/bin/sh
# Serves a status page after a slow start, like an app server booting.
set -eu
version=$(cat /app/VERSION)
if [ "$version" = broken ]; then
  echo "this release does not start"
  exit 1
fi
sleep 2
mkdir -p /www
{
  echo "version=$version"
  echo "commit=${KOROKU_COMMIT_SHORT:-}"
  echo "migrations=$(cat /data/migrations 2>/dev/null | tr '\n' ',')"
} > /www/index.html
echo "serving version=$version"
exec httpd -f -p 8080 -h /www
