#!/bin/sh
# Entry point of the services in compose.yaml.
#
# Compose starts the container as root. This drops to the owner of the
# checkout (the working directory), so that files written to the bind mount
# keep that owner, and adds the group of the Docker socket so that the
# command can talk to the host's daemon. bin/dev does the same with
# `docker run --user`.
set -eu

if [ "$(id -u)" -ne 0 ]; then
    exec "$@"
fi

uid=$(stat -c %u .)
gid=$(stat -c %g .)
groups=$gid
socket=/var/run/docker.sock
if [ -S "$socket" ]; then
    groups="$groups,$(stat -c %g "$socket")"
fi

exec setpriv --reuid "$uid" --regid "$gid" --groups "$groups" "$@"
