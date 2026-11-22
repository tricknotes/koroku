#!/usr/bin/env bash
# Provision a fresh host for koroku: Ubuntu 24.04 or 26.04, amd64, run as root.
#
#   sudo ./provision.sh [options] /path/to/koroku
#
# The koroku binary is built elsewhere with `make build` (bin/koroku). The files
# next to this script (koroku.service, caddy-initial.json, caddy-koroku.conf,
# config.example.toml) are installed as they are, so copy the whole deploy/
# directory to the host.
#
# The script is safe to run again: it only changes what differs, and it never
# overwrites /etc/koroku/config.toml or any key under /etc/koroku. It never
# reboots; it says at the end when a reboot is needed.
#
# See docs/OPERATIONS.md for the full procedure.

# -E: the ERR trap below also fires inside functions.
set -Eeuo pipefail

# --- Settings ---------------------------------------------------------------

# Compose merges the generated override with !override and !reset.
readonly MIN_COMPOSE_VERSION=2.30.0
# The generated Caddy configuration uses stream_close_delay.
readonly MIN_CADDY_VERSION=2.7.0
# restic comes from the official release, pinned by checksum (the reason is in
# docs/OPERATIONS.md). To move to another release, change both values; the
# checksum is in the SHA256SUMS file of that release.
readonly RESTIC_VERSION=0.19.1
readonly RESTIC_SHA256=f415415624dcc452f2a02b8c33641791a8c6d6d3b65bbb3543fcf9a25151585c

# Ubuntu releases (VERSION_ID) the script is tested on, amd64 only.
readonly SUPPORTED_UBUNTU_VERSIONS=(24.04 26.04)

readonly SWAP_FILE=/swapfile
readonly SWAP_SIZE_MIB=2048
# zswap keeps pages on their way to swap compressed in RAM. Its settings are
# written by systemd-tmpfiles at every boot from this file.
readonly ZSWAP_TMPFILES=/etc/tmpfiles.d/koroku-zswap.conf
readonly ZSWAP_PARAMS=/sys/module/zswap/parameters

readonly CONFIG_DIR=/etc/koroku
readonly CONFIG_FILE=$CONFIG_DIR/config.toml
readonly DATA_DIR=/var/lib/koroku
readonly CADDY_LOG_DIR=/var/log/caddy
readonly CADDY_ADMIN_URL=http://127.0.0.1:2019
# sshd reads the files in sshd_config.d in order and keeps the first value of
# each setting, so this name wins over 50-cloud-init.conf and the like.
readonly SSHD_DROP_IN=/etc/ssh/sshd_config.d/01-koroku.conf
readonly SSHD_KEEPALIVE_DROP_IN=/etc/ssh/sshd_config.d/02-koroku-keepalive.conf
# sshd asks an idle client every this many seconds whether it is still there.
readonly SSH_CLIENT_ALIVE_INTERVAL=30
readonly SSH_CLIENT_ALIVE_COUNT_MAX=4

# --- Output -----------------------------------------------------------------

NOTES=()

step() { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
# note prints a message now and repeats it in the final summary.
note() {
  printf '    NOTE: %s\n' "$*"
  NOTES+=("$*")
}
die() {
  printf '\nprovision.sh: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage: provision.sh [options] /path/to/koroku

Sets up Docker, Caddy, restic, swap with zswap, the firewall, key-only SSH and the koroku
service on a fresh Ubuntu 24.04 or 26.04 (amd64) host. Run it as root. It can be run
again safely.

Options:
  --skip-upgrade  Do not upgrade packages that are already installed. Use it
                  to re-apply the configuration on a host with running apps:
                  a Docker upgrade restarts every container.
  --http3         Also open 443/udp so that HTTP/3 reaches Caddy. Off by
                  default; see docs/OPERATIONS.md.
  --keep-ssh-password
                  Leave SSH password login as it is. By default it is turned
                  off once the user who ran sudo has an authorized SSH key.
  --force         Run on a system other than Ubuntu 24.04 or 26.04 amd64
                  (untested).
  -h, --help      Show this help.
EOF
}

# --- Helpers ----------------------------------------------------------------

# install_file SRC DST MODE [OWNER:GROUP]
# Puts a copy of SRC at DST, atomically, when the content differs, and sets
# FILE_CHANGED to 1 or 0. It reports through a variable rather than its exit
# status so that it is never called from a condition, where bash would ignore
# a failing command inside it.
FILE_CHANGED=0
CADDY_DROP_IN_CHANGED=0
install_file() {
  local src=$1 dst=$2 mode=$3 owner=${4:-root:root} tmp
  if [[ -f $dst ]] && cmp -s -- "$src" "$dst"; then
    FILE_CHANGED=0
  else
    tmp=$(mktemp -- "$dst.XXXXXX")
    cat -- "$src" >"$tmp"
    chown -- "$owner" "$tmp"
    chmod -- "$mode" "$tmp"
    mv -f -- "$tmp" "$dst"
    FILE_CHANGED=1
  fi
  chown -- "$owner" "$dst"
  chmod -- "$mode" "$dst"
}

# version_ge A B: true when version A is at least version B.
version_ge() { dpkg --compare-versions "$1" ge "$2"; }

fetch() {
  local url=$1 out=$2
  curl --fail --silent --show-error --location \
    --retry 3 --retry-delay 2 --connect-timeout 15 --max-time 300 \
    --output "$out" "$url"
}

APT_OPTIONS=(
  --yes
  -o Dpkg::Options::=--force-confdef
  -o Dpkg::Options::=--force-confold
  # A fresh host often runs unattended-upgrades right after its first boot.
  -o DPkg::Lock::Timeout=600
)

apt_update() {
  local attempt
  for attempt in 1 2 3 4 5 6; do
    # Error-Mode=any: a repository that cannot be fetched is an error, not a
    # warning that surfaces later as "unable to locate package".
    if apt-get -o APT::Update::Error-Mode=any -o Acquire::Retries=3 update; then
      return 0
    fi
    info "apt-get update failed (attempt $attempt of 6), trying again in 10 s"
    sleep 10
  done
  die "apt-get update keeps failing; see the messages above"
}

apt_install() {
  if [[ $SKIP_UPGRADE == 1 ]]; then
    apt-get "${APT_OPTIONS[@]}" install --no-upgrade "$@"
  else
    apt-get "${APT_OPTIONS[@]}" install "$@"
  fi
}

# --- Steps ------------------------------------------------------------------

check_host() {
  step "Checking the host"
  if [[ $EUID -ne 0 ]]; then
    die "must be run as root (sudo $0 ...)"
  fi

  local id='' version='' codename='' arch=''
  if [[ -r /etc/os-release ]]; then
    # shellcheck source=/dev/null
    id=$(. /etc/os-release && printf '%s' "${ID:-}")
    # shellcheck source=/dev/null
    version=$(. /etc/os-release && printf '%s' "${VERSION_ID:-}")
    # shellcheck source=/dev/null
    codename=$(. /etc/os-release && printf '%s' "${VERSION_CODENAME:-}")
  fi
  if command -v dpkg >/dev/null 2>&1; then
    arch=$(dpkg --print-architecture)
  fi
  info "system: ${id:-unknown} ${version:-unknown} (${codename:-unknown}), ${arch:-unknown}"
  local supported=0 v
  for v in "${SUPPORTED_UBUNTU_VERSIONS[@]}"; do
    if [[ $version == "$v" ]]; then
      supported=1
    fi
  done
  if [[ $id != ubuntu || $supported != 1 || $arch != amd64 ]]; then
    if [[ $FORCE != 1 ]]; then
      die "this script supports Ubuntu ${SUPPORTED_UBUNTU_VERSIONS[*]} on amd64 only (use --force to try anyway)"
    fi
    note "running on an unsupported system because of --force"
  fi
  if [[ -z $codename || -z $arch ]]; then
    die "cannot determine the release codename or the architecture"
  fi
  OS_CODENAME=$codename
  OS_ARCH=$arch

  local file
  for file in koroku.service caddy-initial.json caddy-koroku.conf config.example.toml; do
    if [[ ! -f $SCRIPT_DIR/$file ]]; then
      die "$SCRIPT_DIR/$file is missing; copy the whole deploy/ directory to the host"
    fi
  done

  if [[ ! -f $KOROKU_BINARY ]]; then
    die "$KOROKU_BINARY: no such file (build it with 'make build' and copy bin/koroku here)"
  fi
  if [[ ! -x $KOROKU_BINARY ]]; then
    die "$KOROKU_BINARY is not executable (chmod +x it)"
  fi
  # Running it proves that it is a binary for this machine before anything
  # on the host is changed.
  local version_output
  if ! version_output=$("$KOROKU_BINARY" version 2>&1); then
    die "$KOROKU_BINARY does not run on this machine: $version_output"
  fi
  KOROKU_VERSION=$version_output
  info "koroku binary: $KOROKU_BINARY (version $KOROKU_VERSION)"
}

install_base_packages() {
  step "Installing base packages"
  apt_update
  # curl, gnupg, ca-certificates: package repositories and downloads.
  # git: koroku fetches repositories with it. jq: merges Docker's daemon.json.
  # bzip2: unpacks the restic release. tzdata: the backup schedule is in JST.
  apt_install ca-certificates curl gnupg git jq bzip2 tzdata ufw
}

# Docker's log rotation settings go in before the package is installed, so
# that the daemon starts with them the first time.
configure_docker_daemon() {
  step "Configuring Docker log rotation (/etc/docker/daemon.json)"
  local file=/etc/docker/daemon.json current='{}' merged
  mkdir -p /etc/docker
  if [[ -s $file ]]; then
    if ! jq -e 'type == "object"' "$file" >/dev/null 2>&1; then
      die "$file is not a JSON object; fix or remove it and run this script again"
    fi
    current=$(jq -S . "$file")
  fi
  # Keep every other setting, and other log options, of an existing file.
  merged=$(jq -S '
    .["log-driver"] = "json-file"
    | .["log-opts"] = ((.["log-opts"] // {}) + {"max-size": "10m", "max-file": "3"})
  ' <<<"$current")

  DOCKER_DAEMON_CHANGED=0
  if [[ -f $file && $merged == "$current" ]]; then
    info "already set"
    return 0
  fi
  if [[ -s $file && ! -e $file.before-koroku ]]; then
    cp -p -- "$file" "$file.before-koroku"
    info "kept the previous file as $file.before-koroku"
  fi
  printf '%s\n' "$merged" >"$WORK_DIR/daemon.json"
  install_file "$WORK_DIR/daemon.json" "$file" 0644
  DOCKER_DAEMON_CHANGED=$FILE_CHANGED
  info "written (applies to containers created from now on)"
}

# Caddy's first start must already use the drop-in: the package's default
# configuration serves a welcome page on port 80.
configure_caddy_unit() {
  step "Configuring Caddy for koroku"
  mkdir -p /etc/caddy /etc/systemd/system/caddy.service.d
  install_file "$SCRIPT_DIR/caddy-initial.json" /etc/caddy/koroku-initial.json 0644
  info "/etc/caddy/koroku-initial.json: $(changed_word "$FILE_CHANGED")"
  install_file "$SCRIPT_DIR/caddy-koroku.conf" /etc/systemd/system/caddy.service.d/koroku.conf 0644
  CADDY_DROP_IN_CHANGED=$FILE_CHANGED
  info "/etc/systemd/system/caddy.service.d/koroku.conf: $(changed_word "$FILE_CHANGED")"
}

changed_word() {
  if [[ $1 == 1 ]]; then printf 'installed'; else printf 'unchanged'; fi
}

add_package_repositories() {
  step "Adding the Docker and Caddy package repositories"
  mkdir -p /etc/apt/keyrings /usr/share/keyrings

  # Docker: https://docs.docker.com/engine/install/ubuntu/
  fetch https://download.docker.com/linux/ubuntu/gpg "$WORK_DIR/docker.asc"
  install_file "$WORK_DIR/docker.asc" /etc/apt/keyrings/docker.asc 0644
  printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu %s stable\n' \
    "$OS_ARCH" "$OS_CODENAME" >"$WORK_DIR/docker.list"
  install_file "$WORK_DIR/docker.list" /etc/apt/sources.list.d/docker.list 0644

  # Caddy: https://caddyserver.com/docs/install#debian-ubuntu-raspbian
  # Ubuntu's own caddy package is older than MIN_CADDY_VERSION.
  fetch https://dl.cloudsmith.io/public/caddy/stable/gpg.key "$WORK_DIR/caddy.asc"
  gpg --batch --yes --dearmor --output "$WORK_DIR/caddy.gpg" "$WORK_DIR/caddy.asc"
  install_file "$WORK_DIR/caddy.gpg" /usr/share/keyrings/caddy-stable-archive-keyring.gpg 0644
  printf 'deb [signed-by=/usr/share/keyrings/caddy-stable-archive-keyring.gpg] https://dl.cloudsmith.io/public/caddy/stable/deb/debian any-version main\n' \
    >"$WORK_DIR/caddy.list"
  install_file "$WORK_DIR/caddy.list" /etc/apt/sources.list.d/caddy-stable.list 0644

  apt_update
}

upgrade_system() {
  if [[ $SKIP_UPGRADE == 1 ]]; then
    step "Skipping the system upgrade (--skip-upgrade)"
    return 0
  fi
  step "Upgrading all packages, including the kernel"
  apt-get "${APT_OPTIONS[@]}" full-upgrade
}

install_docker() {
  step "Installing Docker Engine and the Compose plugin"
  local had_docker=0
  if command -v dockerd >/dev/null 2>&1; then
    had_docker=1
  fi
  apt_install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin

  systemctl enable docker.service
  if [[ $had_docker == 1 && $DOCKER_DAEMON_CHANGED == 1 ]]; then
    note "restarted Docker to apply daemon.json; running containers were restarted with it"
    systemctl restart docker.service
  else
    systemctl start docker.service
  fi

  local i ready=0
  for ((i = 0; i < 30; i++)); do
    if docker info >/dev/null 2>&1; then
      ready=1
      break
    fi
    sleep 1
  done
  if [[ $ready != 1 ]]; then
    die "the Docker daemon does not answer; see 'journalctl -u docker'"
  fi

  DOCKER_VERSION=$(docker --version | sed -E 's/^Docker version ([^, ]+).*/\1/')
  COMPOSE_VERSION=$(docker compose version --short)
  COMPOSE_VERSION=${COMPOSE_VERSION#v}
  info "Docker $DOCKER_VERSION, Compose $COMPOSE_VERSION"
  if ! version_ge "$COMPOSE_VERSION" "$MIN_COMPOSE_VERSION"; then
    die "Docker Compose $COMPOSE_VERSION is older than $MIN_COMPOSE_VERSION, which koroku needs for !override and !reset"
  fi
}

caddy_runs_with_resume() {
  local pid cmdline
  pid=$(systemctl show --property MainPID --value caddy.service 2>/dev/null || true)
  if [[ ! $pid =~ ^[1-9][0-9]*$ || ! -r /proc/$pid/cmdline ]]; then
    return 1
  fi
  cmdline=$(tr '\0' ' ' <"/proc/$pid/cmdline")
  [[ $cmdline == *' --resume '* ]]
}

install_caddy() {
  step "Installing Caddy"
  apt_install caddy

  CADDY_VERSION=$(caddy version | awk '{print $1}')
  CADDY_VERSION=${CADDY_VERSION#v}
  info "Caddy $CADDY_VERSION"
  if ! version_ge "$CADDY_VERSION" "$MIN_CADDY_VERSION"; then
    die "Caddy $CADDY_VERSION is older than $MIN_CADDY_VERSION, which koroku needs for stream_close_delay"
  fi

  # One access log per app is written here by Caddy and read by koroku (root).
  if [[ ! -d $CADDY_LOG_DIR ]]; then
    install -d -m 0755 -o caddy -g caddy "$CADDY_LOG_DIR"
  fi
  install -d -m 0750 -o caddy -g caddy "$CADDY_LOG_DIR/apps"

  systemctl daemon-reload
  systemctl enable caddy.service
  if caddy_runs_with_resume && [[ $CADDY_DROP_IN_CHANGED != 1 ]]; then
    info "Caddy already runs with --resume"
  elif caddy_runs_with_resume; then
    # A changed drop-in (for example its environment) applies only to a new
    # process. --resume brings the routes koroku loaded back at once.
    systemctl restart caddy.service
    note "restarted Caddy to apply its changed drop-in; open connections were closed once"
  else
    # Either not running yet, or started before the drop-in existed. With
    # --resume a restart comes back with the configuration koroku loaded last.
    systemctl restart caddy.service
  fi

  local i ready=0
  for ((i = 0; i < 15; i++)); do
    if curl --silent --fail --max-time 3 --output /dev/null "$CADDY_ADMIN_URL/config/"; then
      ready=1
      break
    fi
    sleep 1
  done
  if [[ $ready != 1 ]]; then
    die "the Caddy admin API does not answer on $CADDY_ADMIN_URL; see 'journalctl -u caddy'"
  fi
  info "admin API answers on $CADDY_ADMIN_URL"
}

install_restic() {
  step "Installing restic $RESTIC_VERSION"
  local target=/usr/local/bin/restic have=''
  if [[ -x $target ]]; then
    have=$("$target" version 2>/dev/null | awk 'NR == 1 && $1 == "restic" { print $2 }' || true)
  fi
  if [[ -n $have ]] && version_ge "$have" "$RESTIC_VERSION"; then
    RESTIC_INSTALLED_VERSION=$have
    info "restic $have is already installed"
    return 0
  fi

  local archive=$WORK_DIR/restic.bz2
  # The pinned checksum is the one of the amd64 build.
  fetch "https://github.com/restic/restic/releases/download/v${RESTIC_VERSION}/restic_${RESTIC_VERSION}_linux_amd64.bz2" "$archive"
  if ! printf '%s  %s\n' "$RESTIC_SHA256" "$archive" | sha256sum --check --status; then
    die "the restic download does not match the pinned SHA-256 checksum"
  fi
  bunzip2 --stdout "$archive" >"$WORK_DIR/restic"
  install_file "$WORK_DIR/restic" "$target" 0755
  RESTIC_INSTALLED_VERSION=$("$target" version | awk 'NR == 1 && $1 == "restic" { print $2 }')
  if [[ $RESTIC_INSTALLED_VERSION != "$RESTIC_VERSION" ]]; then
    die "$target reports version '$RESTIC_INSTALLED_VERSION', expected $RESTIC_VERSION"
  fi
  info "installed $target (checksum verified)"
}

active_swap_bytes() {
  local total=0 size
  while read -r size; do
    if [[ $size =~ ^[0-9]+$ ]]; then
      total=$((total + size))
    fi
  done < <(swapon --show=SIZE --bytes --noheadings)
  printf '%s' "$total"
}

swap_file_is_active() {
  local names
  names=$(swapon --show=NAME --noheadings)
  grep -qxF -- "$SWAP_FILE" <<<"$names"
}

setup_swap() {
  step "Setting up swap"
  # A swap area reports slightly less than the size of its file.
  local wanted=$(((SWAP_SIZE_MIB - 1) * 1024 * 1024)) total
  total=$(active_swap_bytes)
  if ((total >= wanted)); then
    info "$((total / 1024 / 1024)) MiB of swap is already active"
  else
    if [[ ! -e $SWAP_FILE ]]; then
      # Built under another name, so that an interrupted run never leaves a
      # half-made file at SWAP_FILE.
      local tmp=$SWAP_FILE.koroku-tmp
      rm -f -- "$tmp"
      (
        umask 077
        if ! fallocate --length "${SWAP_SIZE_MIB}MiB" "$tmp" 2>/dev/null; then
          dd if=/dev/zero of="$tmp" bs=1M count="$SWAP_SIZE_MIB" status=none
        fi
      )
      chmod 0600 "$tmp"
      mkswap "$tmp" >/dev/null
      mv -- "$tmp" "$SWAP_FILE"
      info "created $SWAP_FILE (${SWAP_SIZE_MIB} MiB)"
    fi
    if ! swap_file_is_active; then
      swapon "$SWAP_FILE"
    fi
    info "enabled $SWAP_FILE"
  fi

  # Persist only the file this script manages; other swap is not its business.
  if swap_file_is_active; then
    if awk -v file="$SWAP_FILE" '$1 == file { found = 1 } END { exit !found }' /etc/fstab; then
      info "$SWAP_FILE is already in /etc/fstab"
    else
      printf '%s none swap sw 0 0\n' "$SWAP_FILE" >>/etc/fstab
      info "added $SWAP_FILE to /etc/fstab"
    fi
  fi
}

# zswap: idle apps leave much of their heap untouched, and those pages
# compress well. Compressing them in RAM before they reach the swap file frees
# memory without the disk I/O of plain swap. shrinker_enabled (kernel 6.8 and
# later) moves the coldest compressed pages on to the swap file under
# pressure; the "w-" lines ignore a parameter the kernel does not have.
setup_zswap() {
  step "Enabling zswap"
  cat >"$WORK_DIR/koroku-zswap.conf" <<'CONF'
# Installed by koroku's deploy/provision.sh: compress pages in RAM before they
# are written to swap. Applied at boot by systemd-tmpfiles.
w- /sys/module/zswap/parameters/compressor - - - - zstd
w- /sys/module/zswap/parameters/shrinker_enabled - - - - Y
w- /sys/module/zswap/parameters/enabled - - - - Y
CONF
  mkdir -p /etc/tmpfiles.d
  install_file "$WORK_DIR/koroku-zswap.conf" "$ZSWAP_TMPFILES" 0644
  info "$ZSWAP_TMPFILES: $(changed_word "$FILE_CHANGED")"

  if [[ ! -r $ZSWAP_PARAMS/enabled ]]; then
    note "this kernel has no zswap; swap is used uncompressed"
    return 0
  fi
  # Apply now as well, so that no reboot is needed. Only what differs is
  # written; a value the kernel refuses is reported, not fatal.
  local name value want failed=()
  for name in compressor shrinker_enabled enabled; do
    case $name in
      compressor) want=zstd ;;
      *) want=Y ;;
    esac
    [[ -e $ZSWAP_PARAMS/$name ]] || continue
    value=$(cat -- "$ZSWAP_PARAMS/$name" 2>/dev/null || true)
    if [[ $value != "$want" ]]; then
      if ! { printf '%s' "$want" >"$ZSWAP_PARAMS/$name"; } 2>/dev/null; then
        failed+=("$name=$want")
      fi
    fi
  done
  if ((${#failed[@]} > 0)); then
    note "could not set zswap ${failed[*]} now; check $ZSWAP_PARAMS after a reboot"
  fi
  info "zswap: enabled=$(cat -- "$ZSWAP_PARAMS/enabled" 2>/dev/null || echo '?') compressor=$(cat -- "$ZSWAP_PARAMS/compressor" 2>/dev/null || echo '?')"
}

# Placeholders of config.example.toml: example.com names and the
# documentation address range 203.0.113.0/24, outside comments.
config_has_placeholders() {
  awk '
    !/^[[:space:]]*#/ && /example\.com|203\.0\.113\./ { found = 1 }
    END { exit !found }
  ' "$CONFIG_FILE"
}

install_koroku() {
  step "Installing koroku"
  install -d -m 0700 -o root -g root "$CONFIG_DIR" "$DATA_DIR"

  CONFIG_CREATED=0
  if [[ -e $CONFIG_FILE ]]; then
    info "$CONFIG_FILE exists, left untouched"
  else
    install -m 0600 -o root -g root "$SCRIPT_DIR/config.example.toml" "$CONFIG_FILE"
    CONFIG_CREATED=1
    info "created $CONFIG_FILE from config.example.toml"
  fi

  # Keys are never created, replaced or modified here; only report loose
  # permissions, which koroku refuses for the master key.
  local name file mode
  for name in config.toml master.key restic.key github-app.pem; do
    file=$CONFIG_DIR/$name
    if [[ -e $file ]]; then
      mode=$(stat -c '%a' "$file")
      if [[ $mode != 600 && $mode != 400 ]]; then
        note "$file has mode $mode; run: chmod 600 $file"
      fi
    fi
  done

  install_file "$KOROKU_BINARY" /usr/local/bin/koroku 0755
  KOROKU_BINARY_CHANGED=$FILE_CHANGED
  info "/usr/local/bin/koroku (version $KOROKU_VERSION): $(changed_word "$FILE_CHANGED")"

  install_file "$SCRIPT_DIR/koroku.service" /etc/systemd/system/koroku.service 0644
  KOROKU_UNIT_CHANGED=$FILE_CHANGED
  info "/etc/systemd/system/koroku.service: $(changed_word "$FILE_CHANGED")"
  systemctl daemon-reload
}

setup_firewall() {
  step "Configuring the firewall (ufw)"
  # Never lock out SSH: allow 22, every port sshd is configured for, and the
  # port of the session this script runs in, before the firewall is enabled.
  local ports=(22) port
  if command -v sshd >/dev/null 2>&1; then
    while read -r port; do
      ports+=("$port")
    done < <(sshd -T 2>/dev/null | awk '$1 == "port" { print $2 }')
  fi
  if [[ -n ${SSH_CONNECTION:-} ]]; then
    read -r _ _ _ port <<<"$SSH_CONNECTION"
    ports+=("$port")
  fi
  local seen=' '
  for port in "${ports[@]}"; do
    if [[ $port =~ ^[0-9]+$ && $seen != *" $port "* ]]; then
      seen+="$port "
      ufw allow "$port/tcp"
    fi
  done
  ufw allow 80/tcp
  ufw allow 443/tcp
  if [[ $HTTP3 == 1 ]]; then
    ufw allow 443/udp
  fi
  # These are Ubuntu's defaults; changing them on an active firewall reloads
  # it, so only do it when they differ.
  if ! grep -qx 'DEFAULT_INPUT_POLICY="DROP"' /etc/default/ufw 2>/dev/null; then
    ufw default deny incoming
  fi
  if ! grep -qx 'DEFAULT_OUTPUT_POLICY="ACCEPT"' /etc/default/ufw 2>/dev/null; then
    ufw default allow outgoing
  fi
  local status
  status=$(ufw status)
  if [[ $status == 'Status: active'* ]]; then
    info "ufw is already active"
  else
    ufw --force enable
  fi
  SSH_PORTS=${seen# }
  # Ports that Docker publishes bypass ufw. koroku binds every app port to
  # 127.0.0.1, which is what actually keeps them private.
}

# authorized_keys_of USER prints the authorized_keys file of USER when it
# holds at least one key, and nothing otherwise.
authorized_keys_of() {
  local home file
  home=$(getent passwd "$1" | cut -d: -f6 || true)
  file=$home/.ssh/authorized_keys
  if [[ -n $home && -f $file ]] && grep -qE '^[[:space:]]*[^#[:space:]]' "$file"; then
    printf '%s' "$file"
  fi
}

# An idle SSH session freezes when a router or firewall on the way forgets
# the connection (many drop idle TCP connections after a few minutes). The
# server's keepalive messages keep the connection known to them; a client
# that is really gone is dropped after INTERVAL x COUNT_MAX seconds.
keep_ssh_alive() {
  step "Keeping idle SSH sessions alive"
  if ! command -v sshd >/dev/null 2>&1; then
    info "no sshd on this host"
    return 0
  fi
  {
    echo "# Installed by koroku's provision.sh: keep idle SSH sessions from freezing."
    echo "ClientAliveInterval $SSH_CLIENT_ALIVE_INTERVAL"
    echo "ClientAliveCountMax $SSH_CLIENT_ALIVE_COUNT_MAX"
  } >"$WORK_DIR/sshd-keepalive.conf"
  install -d -m 0755 /etc/ssh/sshd_config.d
  install_file "$WORK_DIR/sshd-keepalive.conf" "$SSHD_KEEPALIVE_DROP_IN" 0644
  if [[ $FILE_CHANGED == 1 ]]; then
    if ! sshd -t 2>"$WORK_DIR/sshd-test.err"; then
      rm -f -- "$SSHD_KEEPALIVE_DROP_IN"
      die "sshd rejected the keepalive setting, so it was removed again: $(cat "$WORK_DIR/sshd-test.err")"
    fi
    # A reload keeps open sessions; this one gets the setting at its next login.
    systemctl try-reload-or-restart ssh.service
    info "installed $SSHD_KEEPALIVE_DROP_IN and reloaded sshd"
  else
    info "$SSHD_KEEPALIVE_DROP_IN: unchanged"
  fi
}

harden_ssh() {
  step "Restricting SSH to key-based login"
  if [[ $KEEP_SSH_PASSWORD == 1 ]]; then
    SSH_STATE='left as it is (--keep-ssh-password)'
    info "$SSH_STATE"
    return 0
  fi
  if ! command -v sshd >/dev/null 2>&1; then
    SSH_STATE='no sshd on this host'
    info "$SSH_STATE"
    return 0
  fi
  # The account that ran sudo is the one that must still get in afterwards.
  local user=${SUDO_USER:-root} keys
  keys=$(authorized_keys_of "$user")
  if [[ -z $keys ]]; then
    SSH_STATE="password login still allowed ($user has no authorized SSH key)"
    note "SSH password login stays enabled: $user has no key in ~/.ssh/authorized_keys. Register one (ssh-copy-id), log in with it, and run this script again"
    return 0
  fi
  local root_login=no
  if [[ $user == root ]]; then
    root_login=prohibit-password
  fi
  {
    echo "# Installed by koroku's provision.sh: log in with SSH keys only."
    echo "# To undo, delete this file and run provision.sh with --keep-ssh-password."
    echo "PasswordAuthentication no"
    echo "KbdInteractiveAuthentication no"
    echo "PermitRootLogin $root_login"
  } >"$WORK_DIR/sshd.conf"
  install -d -m 0755 /etc/ssh/sshd_config.d
  install_file "$WORK_DIR/sshd.conf" "$SSHD_DROP_IN" 0644
  if [[ $FILE_CHANGED == 1 ]]; then
    if ! sshd -t 2>"$WORK_DIR/sshd-test.err"; then
      rm -f -- "$SSHD_DROP_IN"
      die "sshd rejected the new setting, so it was removed again: $(cat "$WORK_DIR/sshd-test.err")"
    fi
    # A reload keeps open sessions, this one included.
    systemctl try-reload-or-restart ssh.service
    info "installed $SSHD_DROP_IN and reloaded sshd"
  else
    info "$SSHD_DROP_IN: unchanged"
  fi
  SSH_STATE="keys only (keys of $user in $keys; root login: $root_login)"
}

start_koroku() {
  step "Starting koroku"
  systemctl enable koroku.service
  KOROKU_STATE=''
  if config_has_placeholders; then
    KOROKU_STATE='not started: /etc/koroku/config.toml still contains example values'
    info "$KOROKU_STATE"
    return 0
  fi
  if systemctl is-active --quiet koroku.service; then
    if [[ $KOROKU_BINARY_CHANGED == 1 || $KOROKU_UNIT_CHANGED == 1 ]]; then
      systemctl restart koroku.service
      KOROKU_STATE='restarted with the new binary or unit'
    else
      KOROKU_STATE='already running, left alone'
    fi
  else
    systemctl start koroku.service
    KOROKU_STATE='started'
  fi
  sleep 3
  if ! systemctl is-active --quiet koroku.service; then
    KOROKU_STATE="FAILED to stay up; see 'journalctl -u koroku -n 50'"
    KOROKU_FAILED=1
  fi
  info "$KOROKU_STATE"
}

reboot_needed() {
  if [[ -e /var/run/reboot-required ]]; then
    return 0
  fi
  # Fallback for hosts without the package that writes reboot-required: a
  # kernel newer than the running one is installed.
  local newest=''
  newest=$(find /boot -maxdepth 1 -name 'vmlinuz-*' -printf '%f\n' 2>/dev/null | sed 's/^vmlinuz-//' | sort -V | tail -n 1 || true)
  [[ -n $newest && $newest != "$(uname -r)" ]]
}

summary() {
  step "Summary"
  info "Docker $DOCKER_VERSION, Compose $COMPOSE_VERSION, Caddy $CADDY_VERSION, restic $RESTIC_INSTALLED_VERSION"
  info "koroku $KOROKU_VERSION: $KOROKU_STATE"
  if [[ $HTTP3 == 1 ]]; then
    info "firewall: tcp ${SSH_PORTS% } (ssh), 80, 443 and udp 443 (HTTP/3)"
  else
    info "firewall: tcp ${SSH_PORTS% } (ssh), 80, 443; udp 443 stays closed (no HTTP/3)"
  fi
  info "ssh: $SSH_STATE"
  if ((${#NOTES[@]} > 0)); then
    printf '\n    Notes:\n'
    printf '      - %s\n' "${NOTES[@]}"
  fi

  cat <<EOF

    Manual steps (details in docs/OPERATIONS.md; skip what is done):
      1. DNS: point *.<base_domain> (or console.<base_domain> and every
         <slug>.<base_domain>) to the public address of this host. If the
         hosting provider has a firewall in front of the host, open
         80/tcp and 443/tcp there as well.
      2. Edit $CONFIG_FILE: base_domain, public_ip, the GitHub App
         values and the object storage values. Put the GitHub App
         private key in $CONFIG_DIR/github-app.pem (mode 0600).
      3. Put the restic repository password in $CONFIG_DIR/restic.key
         (mode 0600) and keep a copy outside this host, together with
         $CONFIG_DIR/master.key (created at the first start of koroku)
         and the object storage credentials.
      4. Start koroku: systemctl restart koroku && journalctl -u koroku -f
      5. Run: koroku check
         It tests DNS, the certificate, the GitHub App, object storage and restic, and
         says what is still missing (docs/SETUP.md goes through each one).
EOF
  if [[ $CONFIG_CREATED == 1 ]]; then
    info "(this run created $CONFIG_FILE; it holds example values only)"
  fi

  if reboot_needed; then
    cat <<EOF

    *** REBOOT REQUIRED ***
    The upgrade installed a new kernel or core libraries (running kernel:
    $(uname -r)). Reboot when convenient:  systemctl reboot
    Docker, Caddy and koroku start again on boot.
EOF
    if [[ -r /var/run/reboot-required.pkgs ]]; then
      info "packages asking for the reboot: $(sort -u /var/run/reboot-required.pkgs | tr '\n' ' ')"
    fi
  else
    printf '\n    No reboot is required.\n'
  fi

  if [[ $KOROKU_FAILED == 1 ]]; then
    die "koroku did not stay up; see 'journalctl -u koroku -n 50'"
  fi
}

# --- Main -------------------------------------------------------------------

FORCE=0
SKIP_UPGRADE=0
HTTP3=0
KEEP_SSH_PASSWORD=0
KOROKU_BINARY=''
while (($# > 0)); do
  case $1 in
    --force) FORCE=1 ;;
    --skip-upgrade) SKIP_UPGRADE=1 ;;
    --http3) HTTP3=1 ;;
    --keep-ssh-password) KEEP_SSH_PASSWORD=1 ;;
    -h | --help)
      usage
      exit 0
      ;;
    -*)
      usage >&2
      die "unknown option: $1"
      ;;
    *)
      if [[ -n $KOROKU_BINARY ]]; then
        usage >&2
        die "only one koroku binary can be given"
      fi
      KOROKU_BINARY=$1
      ;;
  esac
  shift
done
if [[ -z $KOROKU_BINARY ]]; then
  usage >&2
  die "the path of the koroku binary is required"
fi

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
readonly SCRIPT_DIR

# Values filled in by the steps and read by the summary.
OS_CODENAME='' OS_ARCH='' KOROKU_VERSION='' KOROKU_STATE='' KOROKU_FAILED=0
DOCKER_VERSION='' COMPOSE_VERSION='' CADDY_VERSION='' RESTIC_INSTALLED_VERSION=''
DOCKER_DAEMON_CHANGED=0 KOROKU_BINARY_CHANGED=0 KOROKU_UNIT_CHANGED=0 CONFIG_CREATED=0
SSH_PORTS='' SSH_STATE=''

export DEBIAN_FRONTEND=noninteractive
# needrestart (run by apt) must only list services that use outdated
# libraries. Its automatic mode could restart Docker, and every container
# with it, in the middle of this script.
export NEEDRESTART_MODE=l

WORK_DIR=$(mktemp -d)
readonly WORK_DIR
trap 'rm -rf -- "$WORK_DIR"' EXIT
on_error() {
  # Command substitutions run this trap too; report once, from the main shell.
  if [[ $BASHPID == "$$" ]]; then
    printf '\nprovision.sh: "%s" failed at line %s. Fix the cause and run the script again.\n' "$2" "$1" >&2
  fi
}
trap 'on_error "$LINENO" "$BASH_COMMAND"' ERR

check_host
install_base_packages
configure_docker_daemon
configure_caddy_unit
add_package_repositories
upgrade_system
install_docker
install_caddy
install_restic
setup_swap
setup_zswap
install_koroku
setup_firewall
keep_ssh_alive
harden_ssh
start_koroku
summary
