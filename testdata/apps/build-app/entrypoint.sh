#!/bin/sh
# Serves a small status page that the integration tests read.
set -eu
mkdir -p /www /data
echo boot >> /data/boots
{
  echo "version=$(cat /app/VERSION)"
  echo "greeting=${GREETING:-}"
  echo "dotenv=${FROM_REPO_DOTENV:-}"
  echo "tricky=${TRICKY:-}"
  echo "boots=$(wc -l < /data/boots)"
} > /www/index.html
exec httpd -f -p 8080 -h /www
