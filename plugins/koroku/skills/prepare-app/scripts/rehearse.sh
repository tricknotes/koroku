#!/usr/bin/env bash
# Rehearse a koroku deploy of the current Git repository on the local Docker.
#
# It mirrors what koroku does on the server: only files Git would commit are
# used (no .git), an override replaces the web service's ports with a loopback
# binding, drops every other published port, injects the env file into all
# services, adds restart: unless-stopped, a memory limit, oom_score_adj and
# (for production) a memory reservation, and then runs config -> build/pull
# -> up --wait -> an HTTP check that accepts any status below 500. The policy
# check is not reproduced here.
#
# With --blue-green it rehearses an app whose public service koroku swaps
# without downtime: the override copies the public service to <service>-blue
# and <service>-green (extends: { file: ... }), takes the original out of up
# with a profile, and moves the commit variables to a second env file
# (env.release) that only the services recreated on every deploy read (the
# colours, the original, one-shots, services with build: and services whose
# interpolated image: follows the commit, found as koroku does by normalizing
# again with an all-zero commit). It first fails on the Compose shapes koroku refuses for blue/green, starts
# blue with the other services, and then deploys again as koroku does: the
# one-shot services and green with up --no-deps, an HTTP check of green, the
# switch, the other services recreated, a drain wait, and blue stopped and
# removed. There is no Caddy here: the script says which port is live, and a
# loop requests the live port every 0.2s through the second deploy and
# fails the rehearsal if any request fails.
#
# Usage: rehearse.sh --port <container port> [options]
#   --file <path>        Compose file relative to the repository root
#                        (default: compose.yaml, else docker-compose.yml)
#   --service <name>     public service (default: web)
#   --port <port>        container port of the public service (required)
#   --health <path>      health-check path (default: /)
#   --env-file <path>    env file with test values (default: empty)
#   --host-port <port>   loopback port to publish on (default: 28080)
#   --memory <limit>     memory limit for services without one (default: 512m)
#   --environment <env>  staging (default) or production, as in the app's
#                        settings: oom_score_adj 500 / -400, and production
#                        reserves memory (mem_reservation). Rootless Docker
#                        cannot set a negative oom_score_adj; rehearse
#                        production on a rootful Docker or Docker Desktop
#   --reservation <size> production only: memory reserved per service
#                        (default: half its limit; 0 for none). One-shot
#                        services (awaited with service_completed_successfully)
#                        get none
#   --task <svc>:<cmd>   after the check, run a one-off command the way
#                        koroku's tasks do (run --rm --no-deps -T <svc>
#                        sh -c <cmd>); repeatable; a non-zero exit fails
#   --pr <number>        rehearse a review app of that pull request: koroku
#                        then also sets KOROKU_PR_NUMBER
#   --sleep              after the check, put the app to sleep and wake it
#                        the way koroku does for an app that sleeps while
#                        idle (stop, then start --wait), and check again
#   --blue-green         rehearse an app with zero-downtime deploys turned on,
#                        as described above
#   --alt-host-port <port>
#                        blue/green: loopback port of green (default: 28081);
#                        blue uses --host-port
#   --drain <seconds>    blue/green: wait after the switch before stopping the
#                        old colour (default: 10, koroku's default; 0-300)
#   --keep               leave the containers, volumes and work directory
set -euo pipefail

file=""
service="web"
port=""
health="/"
env_src=""
host_port="28080"
memory="512m"
environment="staging"
reservation=""
keep=0
sleep_wake=0
blue_green=0
alt_host_port="28081"
drain="10"
pr=""
tasks=()

die() { echo "rehearse: $*" >&2; exit 2; }

while [ $# -gt 0 ]; do
  case "$1" in
    --file) file="${2:?}"; shift 2 ;;
    --service) service="${2:?}"; shift 2 ;;
    --port) port="${2:?}"; shift 2 ;;
    --health) health="${2:?}"; shift 2 ;;
    --env-file) env_src="${2:?}"; shift 2 ;;
    --host-port) host_port="${2:?}"; shift 2 ;;
    --memory) memory="${2:?}"; shift 2 ;;
    --environment) environment="${2:?}"; shift 2 ;;
    --reservation) reservation="${2:?}"; shift 2 ;;
    --task)
      case "${2:?}" in *:?*) ;; *) die "--task needs <service>:<command>" ;; esac
      tasks+=("$2"); shift 2 ;;
    --pr)
      case "${2:?}" in *[!0-9]*|0*) die "--pr needs a pull request number" ;; esac
      pr="$2"; shift 2 ;;
    --sleep) sleep_wake=1; shift ;;
    --blue-green) blue_green=1; shift ;;
    --alt-host-port) alt_host_port="${2:?}"; shift 2 ;;
    --drain) drain="${2:?}"; shift 2 ;;
    --keep) keep=1; shift ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

[ -n "$port" ] || die "--port is required (the port the public service listens on inside the container)"
case "$port$host_port$alt_host_port" in *[!0-9]*) die "ports must be numbers" ;; esac
if [ "$blue_green" = 1 ] && [ "$host_port" = "$alt_host_port" ]; then
  die "--alt-host-port must differ from --host-port"
fi
case "$drain" in ''|*[!0-9]*) die "--drain must be a number of seconds" ;; esac
[ "$drain" -le 300 ] || die "--drain must be 300 seconds or less"
case "$health" in /*) ;; *) die "--health must start with /" ;; esac
case "$environment" in staging|production) ;; *) die "--environment must be staging or production" ;; esac
if [ -n "$env_src" ] && [ ! -f "$env_src" ]; then die "$env_src does not exist"; fi
if [ "$blue_green" = 1 ]; then
  for task in ${tasks[@]+"${tasks[@]}"}; do
    case "${task%%:*}" in
      "$service-blue"|"$service-green") die "--task: koroku refuses the coloured services for tasks (422); name $service" ;;
    esac
  done
fi

# to_bytes 512m -> 536870912; Compose's own output is already in bytes.
to_bytes() {
  local v="${1,,}"
  v="${v%b}"
  case "$v" in
    *k) echo $(( ${v%k} * 1024 )) ;;
    *m) echo $(( ${v%m} * 1024 * 1024 )) ;;
    *g) echo $(( ${v%g} * 1024 * 1024 * 1024 )) ;;
    *) echo "$v" ;;
  esac
}
is_size() {
  case "${1,,}" in [1-9]*) ;; *) return 1 ;; esac
  case "${1,,}" in *[!0-9kmgb]*) return 1 ;; esac
}
is_size "$memory" || die "--memory must be a size such as 512m"
if [ -n "$reservation" ] && [ "$reservation" != "0" ]; then
  is_size "$reservation" || die "--reservation must be 0 or a size such as 256m"
fi

command -v docker >/dev/null || die "docker is not installed"
command -v curl >/dev/null || die "curl is not installed"

# !override and !reset need Compose 2.24.4 or later.
compose_version="$(docker compose version --short 2>/dev/null)" || die "docker compose (v2) is not available"
compose_version="${compose_version#v}"
IFS=. read -r major minor patch <<EOF
${compose_version%%-*}
EOF
if [ "${major:-0}" -lt 2 ] || { [ "$major" -eq 2 ] && { [ "${minor:-0}" -lt 24 ] || { [ "$minor" -eq 24 ] && [ "${patch:-0}" -lt 4 ]; }; }; }; then
  die "Docker Compose $compose_version is too old; 2.24.4 or later is required"
fi

root="$(git rev-parse --show-toplevel 2>/dev/null)" || die "run this inside a Git repository"

if [ -z "$file" ]; then
  if [ -f "$root/compose.yaml" ]; then file="compose.yaml"
  elif [ -f "$root/docker-compose.yml" ]; then file="docker-compose.yml"
  else die "no compose.yaml or docker-compose.yml at the repository root; pass --file"
  fi
fi
[ -f "$root/$file" ] || die "$file does not exist in $root"

work="$(mktemp -d "${TMPDIR:-/tmp}/koroku-rehearse.XXXXXX")"
work="$(cd "$work" && pwd -P)" # Compose resolves relative paths per file
release="$work/release"
mkdir -p "$release"
project="koroku-rehearse-$(basename "$work" | sed 's/.*\.//' | tr '[:upper:]' '[:lower:]')"

# Copy what Git would commit: tracked and untracked-but-not-ignored files.
# koroku fetches only the commit, without .git, submodules or LFS objects.
(
  cd "$root"
  git ls-files -z --cached --others --exclude-standard |
    while IFS= read -r -d '' path; do
      if [ -e "$path" ] || [ -L "$path" ]; then printf '%s\0' "$path"; fi
    done |
    tar --null -T - -cf -
) | (cd "$release" && tar -xf -)

env_file="$work/env"
if [ -n "$env_src" ]; then
  [ -f "$env_src" ] || die "$env_src does not exist"
  cp "$env_src" "$env_file"
else
  : > "$env_file"
fi
# The variables koroku adds to every release (KOROKU_ is reserved): the
# commit, the app's URL and host (here the loopback port), and a review
# app's pull request. A blue/green app gets the commit in env.release, which
# only the services recreated on every deploy read, so that a deploy that
# changes no app variable leaves the database's container alone.
commit="$(git -C "$root" rev-parse HEAD 2>/dev/null || echo 0000000000000000000000000000000000000000)"
release_env="$work/env.release"
commit_env="$env_file"
if [ "$blue_green" = 1 ]; then commit_env="$release_env"; fi
echo "" >> "$env_file"
{
  echo "KOROKU_COMMIT_SHA=$commit"
  echo "KOROKU_COMMIT_SHORT=${commit:0:7}"
} >> "$commit_env"
{
  echo "KOROKU_APP_URL=http://127.0.0.1:$host_port"
  echo "KOROKU_APP_HOST=127.0.0.1:$host_port"
  if [ -n "$pr" ]; then echo "KOROKU_PR_NUMBER=$pr"; fi
} >> "$env_file"
chmod 600 "$env_file" "$commit_env"

# Interpolation reads both files, like koroku's --env-file.
env_args=(--env-file "$env_file")
if [ "$blue_green" = 1 ]; then env_args+=(--env-file "$release_env"); fi
base=(docker compose -p "$project" -f "$release/$file" "${env_args[@]}")
override="$work/koroku.override.yaml"
dc=("${base[@]}" -f "$override")
# config, build and pull of a blue/green release activate the profile that
# takes the original public service out of up, so that it is checked and
# built; up, start and restart do not.
dc_all=("${dc[@]}")
if [ "$blue_green" = 1 ]; then dc_all+=(--profile koroku-disabled); fi

# Blue/green: images the rehearsal tags per release (koroku-style names, so
# down --rmi local leaves them), and the loop that requests the live port.
release_images=()
traffic_pid=""
traffic_on="$work/traffic.on"
traffic_log="$work/traffic.log"
live_file="$work/live-port"

stop_traffic() {
  rm -f "$traffic_on"
  if [ -n "$traffic_pid" ]; then
    wait "$traffic_pid" 2>/dev/null || true
    traffic_pid=""
  fi
}

cleanup() {
  status=$?
  stop_traffic
  if [ "$keep" -eq 1 ]; then
    echo
    echo "Kept. Compose project: $project (measure it with the tune-app Skill's measure.sh --project $project)"
    echo "Clean up with:"
    echo "  ${dc[*]} down --volumes --remove-orphans --rmi local"
    if [ "${#release_images[@]}" -gt 0 ]; then
      echo "  docker image rm ${release_images[*]}"
    fi
    echo "  rm -rf $work"
  else
    echo
    echo "==> Cleaning up (containers, volumes, built images of $project)"
    if [ -f "$override" ]; then
      "${dc[@]}" down --volumes --remove-orphans --rmi local >/dev/null 2>&1 || true
    fi
    if [ "${#release_images[@]}" -gt 0 ]; then
      docker image rm "${release_images[@]}" >/dev/null 2>&1 || true
    fi
    rm -rf "$work"
  fi
  exit "$status"
}
trap cleanup EXIT

# koroku compares these with the variables saved on its screen and warns
# about missing ones (REQUIRED, or no DEFAULT VALUE) before a deployment.
echo "==> Variables $file references"
if vars="$("${base[@]}" config --variables 2>/dev/null)"; then
  if [ -n "$vars" ]; then
    printf '%s\n' "$vars" | sed 's/^/    /'
  else
    echo "    (none: koroku cannot tell which variables the app needs)"
  fi
else
  echo "    (this Compose cannot list them: docker compose config --variables failed)"
fi

# When koroku promotes a staging release to production, it reuses the
# staging image unless the interpolated build args differ. Args that read
# variables usually differ per environment and force a rebuild (and bake
# the value into the image): list them so they can be moved to runtime.
echo "==> Build args that read variables"
if raw="$("${base[@]}" config --no-interpolate 2>/dev/null)"; then
  args="$(printf '%s\n' "$raw" | awk '
    /^      args:/ { on = 1; next }
    on && !/^        / { on = 0 }
    on && /\$/ { sub(/^ +/, ""); print }')"
  if [ -n "$args" ]; then
    printf '%s\n' "$args" | sed 's/^/    /'
    echo "    (environment-specific values here are baked into the image; read them at runtime instead)"
  else
    echo "    (none)"
  fi
else
  echo "    (docker compose config --no-interpolate failed)"
fi

echo "==> Normalizing $file"
services="$("${base[@]}" config --services)" || die "docker compose config failed (see above)"
printf '%s\n' "$services" | grep -qx -- "$service" ||
  die "service \"$service\" is not defined (services: $(printf '%s\n' "$services" | paste -sd ' ' -))"
normalized="$("${base[@]}" config --no-env-resolution 2>/dev/null)" ||
  die "docker compose config --no-env-resolution failed"

# Each service's part of the normalized Compose, indented as config prints it.
declare -A yaml
for name in $services; do
  yaml[$name]="$("${base[@]}" config --no-env-resolution "$name" 2>/dev/null | awk -v s="  $name:" '
    $0 == s { on = 1; next }
    on && /^  [^ ]/ { on = 0 }
    on && /^[^ ]/ { on = 0 }
    on { print }')"
done
has_build() { printf '%s\n' "${yaml[$1]}" | grep -q '^    build:'; }

# deploy_memory <service yaml> limits|reservations prints
# deploy.resources.<section>.memory in bytes, or nothing.
deploy_memory() {
  printf '%s\n' "$1" | awk -v section="        $2:" '
    $0 == section { on = 1; next }
    on && /^          memory:/ { gsub(/[^0-9]/, "", $2); print $2; exit }
    on && !/^          / { on = 0 }'
}

# One-shot services: those another service waits on with
# condition: service_completed_successfully (migrations, seeds). They have
# exited once the app is up, so koroku reserves no memory for them.
oneshots="$(printf '%s\n' "$normalized" | awk '
  /^    depends_on:$/ { on = 1; next }
  on && /^      [^ ]/ { dep = $1; sub(/:$/, "", dep); gsub(/"/, "", dep); next }
  on && /^        condition: service_completed_successfully$/ { print dep; next }
  on && !/^      / { on = 0 }' | sort -u)"
is_oneshot() { printf '%s\n' "$oneshots" | grep -qx -- "$1"; }

if [ "$blue_green" = 1 ]; then
  # The conditions koroku checks at validate (rule blue_green, which the
  # "relax the checks" setting does not lift).
  echo "==> Checking what blue/green needs"
  problems=()
  web_yaml="${yaml[$service]}"
  if printf '%s\n' "$web_yaml" | grep -q '^    container_name:'; then
    problems+=("$service sets container_name: two copies of it cannot share one name")
  fi
  replicas="$(printf '%s\n' "$web_yaml" | sed -n 's/^      replicas: \([0-9]*\)$/\1/p; s/^    scale: \([0-9]*\)$/\1/p' | tail -n 1)"
  if [ "${replicas:-1}" -gt 1 ]; then
    problems+=("$service sets deploy.replicas to $replicas: blue/green swaps a single container")
  fi
  if is_oneshot "$service"; then
    problems+=("$service is a one-shot service (another service waits for it to complete)")
  fi
  for colour in blue green; do
    if printf '%s\n' "$services" | grep -qx -- "$service-$colour"; then
      problems+=("a service is named $service-$colour: koroku adds a service with that name")
    fi
  done
  # References to the public service, which a profile takes out of up.
  while IFS= read -r ref; do
    if [ -n "$ref" ]; then
      problems+=("$ref: koroku takes $service out of up with a profile, so this reference would find nothing")
    fi
  done <<EOF
$(printf '%s\n' "$normalized" | awk -v pub="$service" '
  /^services:$/ { in_services = 1; next }
  /^[^ ]/ { in_services = 0 }
  !in_services { next }
  /^  [^ ]/ { svc = $1; sub(/:$/, "", svc); gsub(/"/, "", svc); key = ""; next }
  svc == pub { next }
  /^    [^ ]/ {
    key = $1; sub(/:.*$/, "", key)
    if ($0 ~ "^    (network_mode|ipc|pid): \"?service:" pub "\"?$") print svc " sets " key ": service:" pub
    next
  }
  key == "depends_on" && $0 ~ "^      \"?" pub "\"?:" { print svc " depends_on " pub }
  key == "volumes_from" && $0 ~ "^      - \"?(service:)?" pub "(:r[ow])?\"?$" { print svc " has volumes_from " pub }')
EOF
  # extends: { file: } copies the service from the Compose file itself; one
  # that comes from an include: cannot be copied.
  if ! awk -v pub="$service" '
    /^services:[[:space:]]*$/ { on = 1; next }
    on && /^[^[:space:]#]/ { on = 0 }
    on && /^[[:space:]]+[^[:space:]#]/ {
      match($0, /^[[:space:]]+/)
      if (!indent) indent = RLENGTH
      if (RLENGTH == indent) {
        key = substr($0, indent + 1); sub(/[[:space:]]*:.*$/, "", key); gsub(/["\047]/, "", key)
        if (key == pub) found = 1
      }
    }
    END { exit !found }' "$release/$file"; then
    problems+=("$service is not defined in $file itself (an include:?): koroku copies it with extends: { file: $file }")
  fi
  if [ "${#problems[@]}" -gt 0 ]; then
    printf '    - %s\n' "${problems[@]}"
    die "this Compose cannot use blue/green; koroku fails the deployment at validate (rule blue_green)"
  fi
  echo "    (all met)"
fi

# Blue/green: the services without build: whose interpolated image follows
# the commit (image: ...:sha-${KOROKU_COMMIT_SHORT}). koroku normalizes the
# project once more with only the commit variables changed (an all-zero
# commit) and compares the images; those services read env.release.
commit_images=""
if [ "$blue_green" = 1 ]; then
  probe_commit=0000000000000000000000000000000000000000
  if [ "$commit" = "$probe_commit" ]; then probe_commit=ffffffffffffffffffffffffffffffffffffffff; fi
  probe_env="$work/env.probe"
  printf 'KOROKU_COMMIT_SHA=%s\nKOROKU_COMMIT_SHORT=%s\n' "$probe_commit" "${probe_commit:0:7}" > "$probe_env"
  # images_of prints "<service> <image>" from a normalized Compose.
  images_of() {
    awk '
      /^services:$/ { on = 1; next }
      /^[^ ]/ { on = 0 }
      !on { next }
      /^  [^ ]/ { svc = $1; sub(/:$/, "", svc); gsub(/"/, "", svc); next }
      /^    image: / { img = $0; sub(/^    image: /, "", img); print svc " " img }'
  }
  probed_config="$(docker compose -p "$project" -f "$release/$file" --env-file "$env_file" --env-file "$probe_env" \
    config --no-env-resolution 2>/dev/null)" ||
    die "docker compose config with a probe commit failed"
  probed="$(printf '%s\n' "$probed_config" | images_of)"
  actual_images="$(printf '%s\n' "$normalized" | images_of)"
  for name in $services; do
    if has_build "$name"; then continue; fi
    a="$(printf '%s\n' "$actual_images" | awk -v s="$name" '$1 == s { sub(/^[^ ]+ /, ""); print; exit }')"
    b="$(printf '%s\n' "$probed" | awk -v s="$name" '$1 == s { sub(/^[^ ]+ /, ""); print; exit }')"
    if [ "$a" != "$b" ]; then commit_images="$commit_images $name"; fi
  done
  commit_images="${commit_images# }"
  if [ -n "$commit_images" ]; then
    echo "==> Images that follow the commit: ${commit_images// /, }"
    echo "    (they read env.release and are recreated by every deploy, as on koroku)"
  fi
fi
follows_commit() { case " $commit_images " in *" $1 "*) return 0 ;; esac; return 1; }

# The aliases the coloured services carry: the public service's name on
# each of its networks, so that http://web:3000 reaches whichever runs.
web_networks="$(printf '%s\n' "${yaml[$service]}" | awk '
  /^    networks:$/ { on = 1; next }
  on && /^      [^ ]/ { n = $1; sub(/:.*$/, "", n); gsub(/"/, "", n); print n; next }
  on && !/^      / { on = 0 }')"

# env_lines app|release prints env_file; release adds env.release (blue/green).
env_lines() {
  echo "    env_file:"
  echo "      - \"$env_file\""
  if [ "$1" = release ]; then echo "      - \"$release_env\""; fi
}

# memory_lines <service> prints restart, the memory limit, oom_score_adj
# and the reservation for a service, and leaves its limit in bytes in $limit.
memory_lines() {
  local name="$1" svc_yaml="${yaml[$1]}" own_limit own_reservation deploy_limit deploy_reservation bytes
  if ! printf '%s\n' "$svc_yaml" | grep -q '^    restart:'; then
    echo "    restart: unless-stopped"
  fi
  # The repository's own limit and reservation, in bytes (config prints
  # sizes as plain numbers), in either of Compose's two forms.
  own_limit="$(printf '%s\n' "$svc_yaml" | sed -n 's/^    mem_limit: "\{0,1\}\([0-9]*\)"\{0,1\}$/\1/p')"
  own_reservation="$(printf '%s\n' "$svc_yaml" | sed -n 's/^    mem_reservation: "\{0,1\}\([0-9]*\)"\{0,1\}$/\1/p')"
  deploy_limit="$(deploy_memory "$svc_yaml" limits)"
  deploy_reservation="$(deploy_memory "$svc_yaml" reservations)"
  limit="${own_limit:-$deploy_limit}"
  if [ -z "$limit" ]; then
    echo "    mem_limit: \"$memory\""
    limit="$(to_bytes "$memory")"
  fi
  # koroku owns oom_score_adj and the reservation, whatever the
  # repository wrote (ARCHITECTURE 6.2): production is killed last when
  # the host runs out of memory and keeps memory.low; staging first.
  bytes=0
  if [ "$environment" = production ]; then
    echo "    oom_score_adj: -400"
    if is_oneshot "$name"; then
      bytes=0
    elif [ -z "$reservation" ]; then
      bytes=$((limit / 2 / 1048576 * 1048576))
    elif [ "$reservation" != "0" ]; then
      bytes="$(to_bytes "$reservation")"
    fi
    if [ "$bytes" -gt "$limit" ]; then bytes="$limit"; fi
    if [ "$bytes" -lt 6291456 ]; then bytes=0; fi
  else
    echo "    oom_score_adj: 500"
  fi
  if [ "$bytes" -gt 0 ]; then
    if [ -n "$deploy_limit$deploy_reservation" ]; then
      printf '    deploy:\n      resources:\n        reservations:\n          memory: "%s"\n' "$bytes"
    fi
    if [ -n "$own_reservation" ] || [ -z "$deploy_limit$deploy_reservation" ]; then
      echo "    mem_reservation: \"$bytes\""
    fi
  else
    if [ -n "$deploy_reservation" ]; then
      printf '    deploy:\n      resources:\n        reservations:\n          memory: !reset null\n'
    fi
    if [ -n "$own_reservation" ]; then
      echo "    mem_reservation: !reset null"
    fi
  fi
}

# release_image <service> <release> is the per-release image koroku gives a
# service with build: (koroku-<slug>-<service>:r<deploy id>).
release_image() { echo "$project-${1,,}:r$2"; }

# write_override <release> writes the same shape as the override koroku
# generates (internal/compose/override.go; blue/green: ARCHITECTURE 7.2).
write_override() {
  local r="$1" name colour colour_port env_kind net
  {
    echo "# Rehearsal of the override koroku generates."
    echo "services:"
    for name in $services; do
      echo "  \"$name\":"
      if [ "$blue_green" = 0 ]; then
        if [ "$name" = "$service" ]; then
          echo "    ports: !override"
          echo "      - \"127.0.0.1:$host_port:$port\""
        else
          echo "    ports: !reset []"
        fi
        env_lines app
        memory_lines "$name"
        continue
      fi
      # Blue/green: env.release goes to the services every deploy recreates
      # (the colours, the original, one-shots, services with build:, images
      # that follow the commit).
      env_kind=app
      if [ "$name" = "$service" ] || is_oneshot "$name" || has_build "$name" || follows_commit "$name"; then
        env_kind=release
      fi
      if [ "$name" = "$service" ]; then
        # Out of up and start; run (tasks) and build still name it.
        echo '    profiles: ["koroku-disabled"]'
      fi
      if has_build "$name"; then echo "    image: \"$(release_image "$name" "$r")\""; fi
      echo "    ports: !reset []"
      env_lines "$env_kind"
      memory_lines "$name"
      [ "$name" = "$service" ] || continue
      for colour in blue green; do
        colour_port="$host_port"
        if [ "$colour" = green ]; then colour_port="$alt_host_port"; fi
        echo "  \"$name-$colour\":"
        echo "    extends:"
        echo "      file: \"$release/$file\""
        echo "      service: \"$name\""
        echo "    profiles: !reset []"
        if has_build "$name"; then
          # The original builds; both colours run its image.
          echo "    build: !reset null"
          echo "    image: \"$(release_image "$name" "$r")\""
          echo "    pull_policy: never"
        fi
        echo "    ports: !override"
        echo "      - \"127.0.0.1:$colour_port:$port\""
        env_lines release
        if [ -n "$web_networks" ]; then
          echo "    networks:"
          for net in $web_networks; do
            echo "      \"$net\":"
            echo "        aliases:"
            echo "          - \"$name\""
          done
        fi
        memory_lines "$name"
      done
    done
  } > "$override"
}

write_override 1
echo "--- koroku.override.yaml"
sed 's/^/    /' "$override"

echo "==> Validating with the override"
"${dc_all[@]}" config -q || die "the project does not validate with the override"

others=()
recreated=()
built=()
for name in $services; do
  if has_build "$name"; then built+=("$name"); fi
  [ "$name" != "$service" ] || continue
  others+=("$name")
  if ! is_oneshot "$name"; then recreated+=("$name"); fi
done
oneshot_services=()
for name in $oneshots; do
  if printf '%s\n' "$services" | grep -qx -- "$name"; then oneshot_services+=("$name"); fi
done

echo "==> Building and pulling"
# Blue/green: with the profile active the original builds the release
# image; the colours (build: !reset null, pull_policy: never) are neither
# built nor pulled.
"${dc_all[@]}" build
"${dc_all[@]}" pull --ignore-buildable --quiet
if [ "$blue_green" = 1 ]; then
  for name in ${built[@]+"${built[@]}"}; do release_images+=("$(release_image "$name" 1)"); done
fi

show_logs() {
  echo
  echo "Last output of the containers:"
  "${dc[@]}" logs --no-color --tail 30 || true
}

code="000"
# check_http <port> waits up to 60s for any status below 500, like koroku.
check_http() {
  local url="http://127.0.0.1:$1$health"
  echo "==> ${2:+$2: }Checking $url (any status below 500, up to 60s)"
  local deadline=$(( $(date +%s) + 60 ))
  while :; do
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$url" || true)"
    if [ "$code" != "000" ] && [ "$code" -lt 500 ]; then
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      show_logs
      echo
      echo "FAILED: $url did not answer below 500 within 60s (last: $code)" >&2
      return 1
    fi
    sleep 1
  done
}

live_colour=""
live_port="$host_port"
if [ "$blue_green" = 0 ]; then
  echo "==> Starting (up --wait, up to 300s)"
  up_services=()
else
  # Nothing runs yet, so there is nothing to swap with: koroku starts every
  # service with only the colour of web_color (blue when unset).
  live_colour=blue
  echo "==> Starting blue on port $host_port with the other services (up --wait, up to 300s)"
  up_services=(${others[@]+"${others[@]}"} "$service-blue")
fi
if ! "${dc[@]}" up -d --pull missing --remove-orphans --wait --wait-timeout 300 ${up_services[@]+"${up_services[@]}"}; then
  show_logs
  echo
  echo "FAILED: docker compose up did not become ready" >&2
  exit 1
fi
check_http "$live_port" || exit 1

# container_ids prints "<service> <container id>" for the given services.
container_ids() {
  local name
  for name in "$@"; do
    echo "$name $("${dc[@]}" ps -q "$name" 2>/dev/null | head -n 1)"
  done
}

# report_traffic summarizes the requests the loop sent and fails on any
# connection error or 5xx.
report_traffic() {
  local total failed
  total="$(wc -l < "$traffic_log" | tr -d ' ')"
  failed="$(awk '$3 == "000" || $3 >= 500' "$traffic_log" | wc -l | tr -d ' ')"
  echo "==> Requests to the live port during the deploy (every 0.2s)"
  awk -v blue="$host_port" -v green="$alt_host_port" '
    { n[$2]++ }
    END {
      printf "    blue (%s): %d, green (%s): %d\n", blue, n[blue] + 0, green, n[green] + 0
    }' "$traffic_log"
  echo "    $total requests, $failed failed (connection errors or 5xx)"
  if [ "$failed" -gt 0 ]; then
    awk '$3 == "000" || $3 >= 500 { print "    " $1 " port " $2 " -> " $3 }' "$traffic_log" | head -n 20
    return 1
  fi
}

if [ "$blue_green" = 1 ]; then
  echo "    blue answers on 127.0.0.1:$host_port: the live port (koroku's Caddy sends requests here)"

  # The second deploy. koroku fetches and builds the new commit; the
  # rehearsal deploys the same build again under the next release's tags,
  # which recreates the one-shots and the services with build: as koroku's
  # next deploy does.
  echo
  if [ "${#built[@]}" -gt 0 ]; then
    echo "==> Deploying again (release r2: the same build under new tags)"
  else
    echo "==> Deploying again (release r2: the same images)"
  fi
  for name in ${built[@]+"${built[@]}"}; do
    docker tag "$(release_image "$name" 1)" "$(release_image "$name" 2)"
    release_images+=("$(release_image "$name" 2)")
  done
  write_override 2
  "${dc_all[@]}" config -q || die "the project does not validate with the override of r2"

  before="$(container_ids ${recreated[@]+"${recreated[@]}"})"
  printf '%s\n' "$live_port" > "$live_file"
  : > "$traffic_log"
  : > "$traffic_on"
  (
    while [ -f "$traffic_on" ]; do
      p="$(cat "$live_file")"
      c="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:$p$health" || true)"
      echo "$(date +%H:%M:%S) $p $c" >> "$traffic_log"
      sleep 0.2
    done
  ) &
  traffic_pid=$!

  # remove_green undoes a failed start or check the way koroku does: blue,
  # the database and the workers were not touched and keep running.
  remove_green() {
    echo "==> Removing green (blue keeps answering on $host_port)"
    "${dc[@]}" rm --stop --force "$service-green" || true
    stop_traffic
    report_traffic || true
  }

  echo "==> start: Starting green on port $alt_host_port (up --no-deps --wait, up to 300s)"
  # koroku fails the deployment, without starting the new colour, when the
  # host could not hold a second copy of the public service.
  memory_lines "$service" >/dev/null
  if [ -r /proc/meminfo ]; then
    available="$(awk '/^MemAvailable:/ { a = $2 } /^SwapFree:/ { s = $2 } END { printf "%d\n", (a + s) * 1024 }' /proc/meminfo)"
    echo "    memory: $((available >> 20)) MiB available (MemAvailable + SwapFree), $((limit >> 20)) MiB needed for a second $service"
    if [ "$available" -lt "$limit" ]; then
      stop_traffic
      echo "FAILED: Not enough memory to run two $service containers ($((available >> 20)) MiB available, $((limit >> 20)) MiB needed)." >&2
      exit 1
    fi
  fi
  if [ "${#oneshot_services[@]}" -gt 0 ]; then
    echo "    with the one-shot services: ${oneshot_services[*]} (they run while blue still answers)"
  fi
  if ! "${dc[@]}" up -d --no-deps --wait --wait-timeout 300 ${oneshot_services[@]+"${oneshot_services[@]}"} "$service-green"; then
    show_logs
    remove_green
    echo
    echo "FAILED: green did not become ready" >&2
    exit 1
  fi
  if ! check_http "$alt_host_port" check; then
    remove_green
    exit 1
  fi

  echo "==> switch: Switching traffic from blue to green"
  printf '%s\n' "$alt_host_port" > "$live_file"
  live_colour=green
  live_port="$alt_host_port"
  echo "    Requests now reach green on port $alt_host_port (koroku writes web_color and reloads Caddy here)"

  if [ "${#recreated[@]}" -gt 0 ]; then
    echo "==> services: Recreating the other services (up --no-deps --wait, up to 300s)"
    if ! "${dc[@]}" up -d --no-deps --wait --wait-timeout 300 "${recreated[@]}"; then
      show_logs
      stop_traffic
      echo
      echo "FAILED: the other services did not become ready (koroku switches back to blue and restores them)" >&2
      exit 1
    fi
    after="$(container_ids "${recreated[@]}")"
    while read -r name id; do
      if printf '%s\n' "$before" | grep -qx -- "$name $id"; then
        if follows_commit "$name"; then
          echo "    $name: kept (same commit here; koroku recreates it for each new commit)"
        else
          echo "    $name: kept (same container)"
        fi
      else
        echo "    $name: recreated"
      fi
    done <<EOF
$after
EOF
  fi

  echo "==> drain: Stopping blue after ${drain}s"
  sleep "$drain"
  "${dc[@]}" stop "$service-blue"
  "${dc[@]}" rm --force "$service-blue"
  # A few more requests after blue is gone.
  sleep 1
  stop_traffic
  report_traffic || {
    echo
    echo "FAILED: requests failed during the blue/green deploy" >&2
    exit 1
  }
fi

if [ "$sleep_wake" = 1 ]; then
  # koroku stops an idle app with its containers kept, and the next request
  # starts the same containers again (one-shot services run again too).
  # A blue/green app names the live colour and the other services, since
  # start without names would look for both colours.
  echo "==> Sleeping (stop) and waking (start --wait, up to 300s)"
  "${dc[@]}" stop
  start_services=()
  if [ "$blue_green" = 1 ]; then
    start_services=(${others[@]+"${others[@]}"} "$service-$live_colour")
  fi
  start=$(date +%s)
  if ! "${dc[@]}" start --wait --wait-timeout 300 ${start_services[@]+"${start_services[@]}"}; then
    show_logs
    echo
    echo "FAILED: the app did not start again after a sleep" >&2
    exit 1
  fi
  check_http "$live_port" || exit 1
  echo "    woke up in $(( $(date +%s) - start ))s (the first request after a sleep waits this long)"
fi

for task in ${tasks[@]+"${tasks[@]}"}; do
  task_service="${task%%:*}"
  task_command="${task#*:}"
  echo "==> Running a task in $task_service: $task_command"
  # No terminal and no input, like koroku's tasks.
  if ! "${dc[@]}" run --rm --no-deps -T "$task_service" sh -c "$task_command" </dev/null; then
    echo
    echo "FAILED: the task in $task_service exited with a non-zero status" >&2
    exit 1
  fi
done

echo
"${dc[@]}" ps
echo
url="http://127.0.0.1:$live_port$health"
if [ "$blue_green" = 1 ]; then
  echo "OK: $live_colour answered $url with HTTP $code; koroku now routes the app to $live_colour"
else
  echo "OK: $url answered HTTP $code"
fi
