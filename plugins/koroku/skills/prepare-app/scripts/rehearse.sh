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
#                        (default: half its limit; 0 for none)
#   --task <svc>:<cmd>   after the check, run a one-off command the way
#                        koroku's tasks do (run --rm --no-deps -T <svc>
#                        sh -c <cmd>); repeatable; a non-zero exit fails
#   --pr <number>        rehearse a review app of that pull request: koroku
#                        then also sets KOROKU_PR_NUMBER
#   --sleep              after the check, put the app to sleep and wake it
#                        the way koroku does for an app that sleeps while
#                        idle (stop, then start --wait), and check again
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
    --keep) keep=1; shift ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

[ -n "$port" ] || die "--port is required (the port the public service listens on inside the container)"
case "$port$host_port" in *[!0-9]*) die "ports must be numbers" ;; esac
case "$health" in /*) ;; *) die "--health must start with /" ;; esac
case "$environment" in staging|production) ;; *) die "--environment must be staging or production" ;; esac

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
# app's pull request.
commit="$(git -C "$root" rev-parse HEAD 2>/dev/null || echo 0000000000000000000000000000000000000000)"
{
  echo ""
  echo "KOROKU_COMMIT_SHA=$commit"
  echo "KOROKU_COMMIT_SHORT=${commit:0:7}"
  echo "KOROKU_APP_URL=http://127.0.0.1:$host_port"
  echo "KOROKU_APP_HOST=127.0.0.1:$host_port"
  if [ -n "$pr" ]; then echo "KOROKU_PR_NUMBER=$pr"; fi
} >> "$env_file"
chmod 600 "$env_file"

base=(docker compose -p "$project" -f "$release/$file" --env-file "$env_file")
override="$work/koroku.override.yaml"
dc=("${base[@]}" -f "$override")

cleanup() {
  status=$?
  if [ "$keep" -eq 1 ]; then
    echo
    echo "Kept. Compose project: $project (measure it with the tune-app Skill's measure.sh --project $project)"
    echo "Clean up with:"
    echo "  ${dc[*]} down --volumes --remove-orphans --rmi local"
    echo "  rm -rf $work"
  else
    echo
    echo "==> Cleaning up (containers, volumes, built images of $project)"
    if [ -f "$override" ]; then
      "${dc[@]}" down --volumes --remove-orphans --rmi local >/dev/null 2>&1 || true
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

# deploy_memory <service yaml> limits|reservations prints
# deploy.resources.<section>.memory in bytes, or nothing.
deploy_memory() {
  printf '%s\n' "$1" | awk -v section="        $2:" '
    $0 == section { on = 1; next }
    on && /^          memory:/ { gsub(/[^0-9]/, "", $2); print $2; exit }
    on && !/^          / { on = 0 }'
}

# Same shape as the override koroku generates (internal/compose/override.go).
{
  echo "# Rehearsal of the override koroku generates."
  echo "services:"
  for name in $services; do
    svc_yaml="$("${base[@]}" config --no-env-resolution "$name" 2>/dev/null | awk -v s="  $name:" '
      $0 == s { on = 1; next }
      on && /^  [^ ]/ { on = 0 }
      on && /^[^ ]/ { on = 0 }
      on { print }')"
    echo "  \"$name\":"
    if [ "$name" = "$service" ]; then
      echo "    ports: !override"
      echo "      - \"127.0.0.1:$host_port:$port\""
    else
      echo "    ports: !reset []"
    fi
    echo "    env_file:"
    echo "      - \"$env_file\""
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
      if [ -z "$reservation" ]; then
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
  done
} > "$override"
echo "--- koroku.override.yaml"
sed 's/^/    /' "$override"

echo "==> Validating with the override"
"${dc[@]}" config -q || die "the project does not validate with the override"

echo "==> Building and pulling"
"${dc[@]}" build
"${dc[@]}" pull --ignore-buildable --quiet

show_logs() {
  echo
  echo "Last output of the containers:"
  "${dc[@]}" logs --no-color --tail 30 || true
}

echo "==> Starting (up --wait, up to 300s)"
if ! "${dc[@]}" up -d --remove-orphans --wait --wait-timeout 300; then
  show_logs
  echo
  echo "FAILED: docker compose up did not become ready" >&2
  exit 1
fi

url="http://127.0.0.1:$host_port$health"
code="000"
# check_http waits up to 60s for any status below 500, like koroku.
check_http() {
  echo "==> Checking $url (any status below 500, up to 60s)"
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
      exit 1
    fi
    sleep 1
  done
}
check_http

if [ "$sleep_wake" = 1 ]; then
  # koroku stops an idle app with its containers kept, and the next request
  # starts the same containers again (one-shot services run again too).
  echo "==> Sleeping (stop) and waking (start --wait, up to 300s)"
  "${dc[@]}" stop
  start=$(date +%s)
  if ! "${dc[@]}" start --wait --wait-timeout 300; then
    show_logs
    echo
    echo "FAILED: the app did not start again after a sleep" >&2
    exit 1
  fi
  check_http
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
echo "OK: $url answered HTTP $code"
