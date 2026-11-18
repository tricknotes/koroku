#!/usr/bin/env bash
# Runs inside the container started by dry-run.sh. Not for a real host.
#
# Stand-ins on PATH replace what a container does not have:
#   systemctl  logs the call; "start/restart caddy" runs the ExecStart line of
#              the installed drop-in as the caddy user, "start/restart paasd"
#              runs the ExecStart line of the installed unit, so both are
#              exercised for real
#   docker     answers "docker info"; everything else is the real CLI
#   swapon     remembers that the swap file was enabled
#   ufw        logs the call and remembers "enable"
#   sshd       reports two configured ports

set -euo pipefail

readonly SHIMS=/opt/shims STATE=/run/shims CALLS=/var/log/shim-calls.log
mkdir -p "$SHIMS" "$STATE"
: >"$CALLS"

cat >"$SHIMS/systemctl" <<'SHIM'
#!/usr/bin/env bash
set -euo pipefail
STATE=/run/shims
echo "systemctl $*" >>/var/log/shim-calls.log

pid_of() { cat "$STATE/$1.pid" 2>/dev/null || echo 0; }
alive() {
  local pid
  pid=$(pid_of "$1")
  [[ $pid != 0 ]] && kill -0 "$pid" 2>/dev/null
}
stop() {
  if alive "$1"; then
    kill "$(pid_of "$1")"
    while alive "$1"; do sleep 0.2; done
  fi
}
start() {
  local exec_start
  if alive "$1"; then
    return 0
  fi
  case $1 in
    caddy.service)
      exec_start=$(sed -n 's/^ExecStart=\(\/.*\)$/\1/p' /etc/systemd/system/caddy.service.d/paasd.conf)
      # shellcheck disable=SC2086  # the unit's command line, split on spaces
      HOME=/var/lib/caddy setsid runuser -u caddy -- $exec_start >>/var/log/caddy-run.log 2>&1 &
      echo $! >"$STATE/$1.pid"
      ;;
    paasd.service)
      exec_start=$(sed -n 's/^ExecStart=\(\/.*\)$/\1/p' /etc/systemd/system/paasd.service)
      # shellcheck disable=SC2086
      HOME=/root setsid $exec_start >>/var/log/paasd-run.log 2>&1 &
      echo $! >"$STATE/$1.pid"
      ;;
  esac
}

case ${1:-} in
  start) start "$2" ;;
  restart)
    stop "$2"
    start "$2"
    ;;
  is-active) alive "${@: -1}" ;;
  show)
    # systemctl show --property MainPID --value caddy.service
    # runuser stays the parent of caddy; report the caddy process itself.
    parent=$(pid_of "${@: -1}")
    child=$(pgrep -P "$parent" 2>/dev/null | head -n 1 || true)
    echo "${child:-$parent}"
    ;;
esac
SHIM

cat >"$SHIMS/docker" <<'SHIM'
#!/usr/bin/env bash
if [[ ${1:-} == info ]]; then
  echo "docker info (stand-in: no daemon in the container)" >>/var/log/shim-calls.log
  exit 0
fi
exec /usr/bin/docker "$@"
SHIM

cat >"$SHIMS/swapon" <<'SHIM'
#!/usr/bin/env bash
echo "swapon $*" >>/var/log/shim-calls.log
case "$*" in
  *--show=SIZE*) if [[ -e /run/shims/swap-on ]]; then echo $(($(stat -c %s /swapfile) - 4096)); fi ;;
  *--show=NAME*) if [[ -e /run/shims/swap-on ]]; then echo /swapfile; fi ;;
  /swapfile) touch /run/shims/swap-on ;;
esac
SHIM

cat >"$SHIMS/ufw" <<'SHIM'
#!/usr/bin/env bash
echo "ufw $*" >>/var/log/shim-calls.log
case "$*" in
  status) if [[ -e /run/shims/ufw-on ]]; then echo "Status: active"; else echo "Status: inactive"; fi ;;
  "--force enable") touch /run/shims/ufw-on ;;
esac
SHIM

cat >"$SHIMS/sshd" <<'SHIM'
#!/usr/bin/env bash
printf 'port 22\nport 2222\n'
SHIM

chmod +x "$SHIMS"/*
export PATH=$SHIMS:$PATH
# The session port must be allowed too (third port besides 22 and 2222).
export SSH_CONNECTION="198.51.100.7 50000 192.0.2.10 2200"

provision() { bash /src/deploy/provision.sh "$@"; }

failures=0
check() {
  local description=$1
  shift
  if "$@" >/dev/null 2>&1; then
    echo "ok    $description"
  else
    echo "FAIL  $description"
    failures=$((failures + 1))
  fi
}
not() { ! "$@"; }
called() { grep -qxF -- "$1" "$CALLS"; }
paasd_start_requested() { grep -qE '^systemctl (re)?start paasd\.service$' "$CALLS"; }
mode_is() { [[ $(stat -c '%U:%G %a' "$1") == "$2" ]]; }
caddy_config() { curl -fsS http://127.0.0.1:2019/config/ | jq -c .; }
caddy_has_initial_config_only() { [[ $(caddy_config) == '{"admin":{"listen":"127.0.0.1:2019"}}' ]]; }
caddy_has_console_route() { [[ $(caddy_config) == *console.paas.test* ]]; }
apt_candidate() { apt-cache policy "$1" | awk '/Candidate:/ { print $2 }'; }
apt_restic_is_older_than_pinned() { dpkg --compare-versions "$(apt_candidate restic)" lt 0.17; }
only_config_in_etc_paas() { [[ $(find /etc/paas -type f) == /etc/paas/config.toml ]]; }
swap_file_ok() {
  [[ $(stat -c '%s %a' /swapfile) == '2147483648 600' ]] && grep -qx '/swapfile none swap sw 0 0' /etc/fstab
}
ufw_order_ok() {
  local calls
  calls=$(grep '^ufw' "$CALLS" | tr '\n' ' ')
  [[ $calls == *'ufw allow 22/tcp '*'ufw allow 2222/tcp '*'ufw allow 2200/tcp '*'ufw allow 80/tcp '*'ufw allow 443/tcp '*'ufw --force enable'* ]]
}
refuses_invalid_daemon_json() {
  local refused=0 kept
  cp /etc/docker/daemon.json /tmp/daemon.json.good
  echo 'not json' >/etc/docker/daemon.json
  if ! provision --skip-upgrade /src/paasd; then
    refused=1
  fi
  kept=$(cat /etc/docker/daemon.json)
  cp /tmp/daemon.json.good /etc/docker/daemon.json
  [[ $refused == 1 && $kept == 'not json' ]]
}

snapshot() {
  sha256sum /etc/docker/daemon.json /etc/paas/* /etc/fstab \
    /etc/systemd/system/paasd.service /etc/systemd/system/caddy.service.d/paasd.conf \
    /etc/caddy/paasd-initial.json /etc/apt/sources.list.d/docker.list \
    /etc/apt/sources.list.d/caddy-stable.list /etc/apt/keyrings/docker.asc \
    /usr/share/keyrings/caddy-stable-archive-keyring.gpg \
    /usr/local/bin/paasd /usr/local/bin/restic
  stat -c '%n %a %U:%G' /etc/paas /etc/paas/* /var/lib/paas /var/log/caddy/apps /swapfile \
    /etc/docker/daemon.json /usr/local/bin/paasd /usr/local/bin/restic
}

# An existing daemon.json with unrelated settings must survive the merge.
mkdir -p /etc/docker
echo '{"dns": ["192.0.2.53"], "log-opts": {"labels": "app"}}' >/etc/docker/daemon.json

echo "######## run 1: fresh system ########"
provision /src/paasd
echo
echo "######## checks after run 1 ########"
check "apt's restic is older than 0.17 (reason for the release binary)" apt_restic_is_older_than_pinned
check "daemon.json keeps dns" jq -e '.dns == ["192.0.2.53"]' /etc/docker/daemon.json
check "daemon.json sets json-file with rotation and keeps other log options" \
  jq -e '.["log-driver"] == "json-file" and .["log-opts"] == {"labels": "app", "max-file": "3", "max-size": "10m"}' /etc/docker/daemon.json
check "previous daemon.json kept as backup" test -f /etc/docker/daemon.json.before-paasd
check "docker was started, not restarted (first install)" not called "systemctl restart docker.service"
check "/etc/paas is root-only" mode_is /etc/paas "root:root 700"
check "/var/lib/paas is root-only" mode_is /var/lib/paas "root:root 700"
check "config.toml created as 0600" mode_is /etc/paas/config.toml "root:root 600"
check "config.toml equals the example" cmp /etc/paas/config.toml /src/deploy/config.example.toml
check "no key was created by the script" only_config_in_etc_paas
check "access log directory belongs to caddy" mode_is /var/log/caddy/apps "caddy:caddy 750"
check "caddy user can write an access log" runuser -u caddy -- touch /var/log/caddy/apps/probe.log
check "swap file is 2 GiB, 0600, in fstab" swap_file_ok
check "Caddy runs the initial configuration: admin API only" caddy_has_initial_config_only
check "Caddy serves nothing on port 80" not curl -s --max-time 2 -o /dev/null http://127.0.0.1:80/
check "ufw: ssh ports 22, 2222 (sshd), 2200 (session), then 80 and 443, before enable" ufw_order_ok
check "ufw: 443/udp stays closed by default" not called "ufw allow 443/udp"
check "paasd enabled" called "systemctl enable paasd.service"
check "paasd not started while the config holds example values" not paasd_start_requested

# What the human does next: real values and keys.
sed -i -e 's/dev\.example\.com/paas.test/' -e 's/203\.0\.113\.10/192.0.2.10/' \
  -e 's#^acme_ca = .*#acme_ca = "https://127.0.0.1:9/directory"#' /etc/paas/config.toml
(
  umask 077
  head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' >/etc/paas/master.key
  echo >>/etc/paas/master.key
  echo "restic-password-stand-in" >/etc/paas/restic.key
  echo "pem-stand-in" >/etc/paas/github-app.pem
)
snapshot >/tmp/before
: >"$CALLS"

echo
echo "######## run 2: same command again, config filled in ########"
provision /src/paasd
echo
echo "######## checks after run 2 ########"
snapshot >/tmp/after2
check "no managed file, key or permission changed" diff /tmp/before /tmp/after2
check "docker not restarted" not called "systemctl restart docker.service"
check "caddy not restarted (already runs with --resume)" not called "systemctl restart caddy.service"
check "swap file not enabled twice" not called "swapon /swapfile"
check "ufw not enabled twice" not called "ufw --force enable"
check "paasd started now that the config has no example values" called "systemctl start paasd.service"
sleep 2
# Any HTTP answer will do: without a [github] section the API returns 401.
check "paasd answers on its loopback port" curl -sS -o /dev/null http://127.0.0.1:8080/api/system
check "paasd loaded its configuration into Caddy" caddy_has_console_route
check "Caddy saved it for --resume" test -s /var/lib/caddy/.config/caddy/autosave.json

echo
echo "######## Caddy restart while paasd is down ########"
kill "$(cat "$STATE/paasd.service.pid")"
sleep 1
systemctl restart caddy.service
sleep 2
check "paasd is down" not systemctl is-active paasd.service
check "after a restart Caddy resumes the configuration paasd loaded" caddy_has_console_route
systemctl start paasd.service
sleep 2
: >"$CALLS"

echo
echo "######## run 3: everything already running, --skip-upgrade --http3 ########"
provision --skip-upgrade --http3 /src/paasd
echo
echo "######## checks after run 3 ########"
snapshot >/tmp/after3
check "no managed file, key or permission changed" diff /tmp/before /tmp/after3
check "paasd neither started nor restarted" not paasd_start_requested
check "caddy not restarted" not called "systemctl restart caddy.service"
check "--http3 opens 443/udp" called "ufw allow 443/udp"

echo
echo "######## refusals ########"
check "refuses a file that is not a runnable binary" not provision /etc/hostname
check "refuses to run without the binary argument" not provision
check "refuses an invalid daemon.json instead of overwriting it" refuses_invalid_daemon_json

echo
echo "######## versions ########"
/usr/bin/docker --version
/usr/bin/docker compose version
caddy version
restic version
echo "apt candidate for restic: $(apt_candidate restic)"
echo "apt candidate for caddy:  $(apt_candidate caddy)"
echo "packaged caddy.service:"
grep -E '^(Exec(Start|Reload)|User|ProtectSystem)=' /usr/lib/systemd/system/caddy.service
echo "/var/log/caddy: $(stat -c '%U:%G %a' /var/log/caddy)"
echo "daemon.json:"
cat /etc/docker/daemon.json

echo
if ((failures > 0)); then
  echo "$failures check(s) FAILED"
  exit 1
fi
echo "all checks passed"
