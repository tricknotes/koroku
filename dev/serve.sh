#!/bin/sh
# Runs koroku for local development (the koroku service of compose.yaml).
#
# Writes a development configuration on first use, builds the binary from
# the checkout and runs it with login disabled. Run it from the repository
# root, inside the development container.
set -eu

root=$(pwd)
state="$root/data/dev"
config="$state/config.toml"

if [ ! -f "$config" ]; then
    mkdir -p "$state"
    # The configuration may hold credentials later; koroku insists on 0600.
    (
        umask 077
        cat >"$config" <<EOF
# Development configuration, written by dev/serve.sh. Edit freely; it is not
# overwritten. See deploy/config.example.toml for every setting.
base_domain = "dev.localhost"
listen = "127.0.0.1:8080"
data_dir = "$state/var"
master_key_file = "$state/master.key"
timezone = "Asia/Tokyo"

[deploy]
# Lets an app use a file:// repository URL, for example a local clone.
allow_file_repos = true

[caddy]
# The caddy service of compose.yaml: loopback only, certificates from Caddy's
# local CA. Apps answer on https://<slug>.dev.localhost. Set enabled = false
# to run without Caddy (apps then answer on their host port only).
enabled = true
admin_url = "http://127.0.0.1:2019"
listen_ip = "127.0.0.1"
internal_ca = true
# Caddy runs as root in its container; its log files would not be readable here.
access_log_dir = ""
EOF
    )
    echo "dev/serve.sh: wrote $config" >&2
fi

# The frontend is served by the Vite dev server, but the binary embeds
# web/dist, so the directory has to exist.
mkdir -p web/dist
[ -n "$(ls -A web/dist)" ] || touch web/dist/.keep

# A linked worktree's VCS metadata is not always reachable from here.
bin=/tmp/koroku-dev
go build -buildvcs=false -o "$bin" ./cmd/koroku

exec "$bin" serve --config "$config" --insecure-no-auth
