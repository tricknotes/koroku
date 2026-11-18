#!/usr/bin/env bash
# Dry run of provision.sh in a disposable ubuntu:24.04 container.
#
#   deploy/test/dry-run.sh /path/to/paasd
#
# What it proves: the package repositories resolve on 24.04, the installed
# versions pass the checks, files are merged and installed as intended, a
# second and third run change nothing, and Caddy really comes back with the
# configuration paasd loaded (--resume).
#
# What it cannot prove: anything that needs systemd, a kernel or a network
# interface of a real host. systemctl, swapon, ufw, sshd and "docker info" are
# replaced by stand-ins (see in-container.sh). The container is removed when
# the run ends; nothing else on this machine is touched.

set -euo pipefail

if (($# != 1)); then
  echo "usage: $0 /path/to/paasd" >&2
  exit 2
fi
binary=$(realpath -- "$1")
deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)

exec docker run --rm --init --name "paasd-provision-dry-run-$$" \
  --volume "$deploy_dir:/src/deploy:ro" \
  --volume "$binary:/src/paasd:ro" \
  ubuntu:24.04 bash /src/deploy/test/in-container.sh
