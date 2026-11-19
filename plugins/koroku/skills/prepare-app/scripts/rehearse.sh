#!/usr/bin/env bash
# Rehearse a koroku deploy of the current Git repository on the local Docker.
#
# It mirrors what koroku does on the server: only files Git would commit are
# used (no .git), an override replaces the web service's ports with a loopback
# binding, drops every other published port, injects the env file into all
# services, adds restart: unless-stopped and a memory limit, and then runs
# config -> build/pull -> up --wait -> an HTTP check that accepts any status
# below 500. The policy check is not reproduced here.
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
#   --keep               leave the containers, volumes and work directory
set -euo pipefail

file=""
service="web"
port=""
health="/"
env_src=""
host_port="28080"
memory="512m"
keep=0

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
    --keep) keep=1; shift ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

[ -n "$port" ] || die "--port is required (the port the public service listens on inside the container)"
case "$port$host_port" in *[!0-9]*) die "ports must be numbers" ;; esac
case "$health" in /*) ;; *) die "--health must start with /" ;; esac

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
chmod 600 "$env_file"

base=(docker compose -p "$project" -f "$release/$file" --env-file "$env_file")
override="$work/koroku.override.yaml"
dc=("${base[@]}" -f "$override")

cleanup() {
  status=$?
  if [ "$keep" -eq 1 ]; then
    echo
    echo "Kept. Clean up with:"
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

echo "==> Normalizing $file"
services="$("${base[@]}" config --services)" || die "docker compose config failed (see above)"
printf '%s\n' "$services" | grep -qx -- "$service" ||
  die "service \"$service\" is not defined (services: $(printf '%s\n' "$services" | paste -sd ' ' -))"

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
    if ! printf '%s\n' "$svc_yaml" | grep -Eq '^    mem_limit:|^ +memory:'; then
      echo "    mem_limit: \"$memory\""
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
echo "==> Checking $url (any status below 500, up to 60s)"
deadline=$(( $(date +%s) + 60 ))
code="000"
while :; do
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$url" || true)"
  if [ "$code" != "000" ] && [ "$code" -lt 500 ]; then
    break
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    show_logs
    echo
    echo "FAILED: $url did not answer below 500 within 60s (last: $code)" >&2
    exit 1
  fi
  sleep 1
done

echo
"${dc[@]}" ps
echo
echo "OK: $url answered HTTP $code"
