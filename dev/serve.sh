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

# data/ holds local state inside the module. A go.mod makes it a module of
# its own, which keeps `go test ./...` from walking into it (volume restores
# leave directories there that only root or the app's users can read).
mkdir -p "$root/data"
if [ ! -f "$root/data/go.mod" ]; then
    printf '%s\n' '// Local state of dev/serve.sh, kept out of the koroku module.' 'module koroku.local/data' >"$root/data/go.mod"
fi

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

[storage]
# The storage service of compose.yaml (Versity S3 Gateway), in place of Cloudflare R2.
endpoint = "http://127.0.0.1:9000"
region = "us-east-1"
bucket = "koroku"
access_key_id = "koroku-dev"
secret_access_key = "koroku-dev-secret"

[backup]
restic_password_file = "$state/restic.key"
EOF
    )
    echo "dev/serve.sh: wrote $config" >&2
fi

# The restic repository password. koroku enables backups only when the file
# exists; losing it makes the local backups unreadable (delete the
# koroku-dev-storage-data volume to start over).
if [ ! -f "$state/restic.key" ]; then
    (
        umask 077
        head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$state/restic.key"
    )
    echo "dev/serve.sh: wrote $state/restic.key" >&2
fi

# The frontend is served by the Vite dev server, but the binary embeds
# web/dist, so the directory has to exist.
mkdir -p web/dist
[ -n "$(ls -A web/dist)" ] || touch web/dist/.keep

# A linked worktree's VCS metadata is not always reachable from here.
bin=/tmp/koroku-dev
go build -buildvcs=false -o "$bin" ./cmd/koroku

exec "$bin" serve --config "$config" --insecure-no-auth
