#!/bin/sh
# Applies the migrations the release lists that the data lacks: they only
# add, so the previous release keeps working while the new one starts.
set -eu
touch /data/migrations
while read -r migration; do
  [ -n "$migration" ] || continue
  grep -qx "$migration" /data/migrations || echo "$migration" >> /data/migrations
done < /app/MIGRATIONS
echo "migrations: $(wc -l < /data/migrations)"
