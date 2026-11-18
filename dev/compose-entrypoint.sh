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

# DEV_FILE_CAPS keeps the capabilities that let the process read and write
# files it does not own (and set their owner), as root would on a real host.
# koroku needs them to back up and restore the named volumes of apps, whose
# files belong to root or to the users of the app's images. Files it creates
# still belong to the owner of the checkout.
if [ "${DEV_FILE_CAPS:-}" = 1 ]; then
    caps=+chown,+dac_override,+fowner,+fsetid
    exec setpriv --reuid "$uid" --regid "$gid" --groups "$groups" \
        --inh-caps "$caps" --ambient-caps "$caps" "$@"
fi

exec setpriv --reuid "$uid" --regid "$gid" --groups "$groups" "$@"
