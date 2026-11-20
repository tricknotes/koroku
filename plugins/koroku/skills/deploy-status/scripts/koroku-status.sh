#!/usr/bin/env bash
# Read the state of koroku apps with a read-only API token.
#
# Needs KOROKU_URL (the console, e.g. https://console.dev.example.com) and
# KOROKU_TOKEN (koroku_ro_..., created on the console's security page) in
# the environment. The token is passed to curl on stdin, never as an
# argument, so it does not show up in the process list; it is never printed.
# Uses curl; jq is optional (without it, the API's JSON is printed as is).
#
# Usage: koroku-status.sh [command] [options]
#   status [--app <slug>] [--all] [--lines N]
#       (default) apps deployed from this repository (matched on the
#       origin remote), their latest deployment and, when it failed, the
#       tail of the deployment log, the services, the last runtime log
#       lines and missing environment variables.
#   apps                       every app, one per line
#   deployments <slug>         recent deployments of an app
#   log <deployment-id> [--lines N]
#                              the deployment log (last N lines; 0 = all)
#   runtime <slug> [--service <name>] [--lines N]
#                              the last runtime log lines (default 100)
#   env <slug> [--ref <branch>|--sha <sha>]
#                              variables the repository needs vs. those set
#                              (keys only; values are never available)
set -euo pipefail

die() { echo "koroku-status: $*" >&2; exit 2; }

[ -n "${KOROKU_URL:-}" ] || die "KOROKU_URL is not set (the console URL, e.g. https://console.dev.example.com)"
[ -n "${KOROKU_TOKEN:-}" ] || die "KOROKU_TOKEN is not set (create a read-only token on the console: セキュリティ > API トークン)"
case "$KOROKU_TOKEN" in
  koroku_ro_*) ;;
  *) die "KOROKU_TOKEN does not look like a koroku token (it starts with koroku_ro_)" ;;
esac
base="${KOROKU_URL%/}"
case "$base" in
  https://*) ;;
  http://localhost:* | http://localhost | http://127.0.0.1:* | http://127.0.0.1 | http://*.localhost:* | http://*.localhost) ;;
  *) die "KOROKU_URL must use https:// (plain http is accepted only for localhost)" ;;
esac
command -v curl >/dev/null || die "curl is required"
have_jq=0
if command -v jq >/dev/null; then have_jq=1; fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# api <path> [curl options...]: GET a path; the body goes to stdout. Exits
# with a readable message on API errors.
api() {
  local path="$1" code
  shift
  code="$(printf 'header = "Authorization: Bearer %s"\n' "$KOROKU_TOKEN" |
    curl -sS -K - -H 'Accept: application/json' -o "$tmp/body" -w '%{http_code}' "$@" "$base$path")" ||
    die "could not reach $base ($path)"
  case "$code" in
    2??) cat "$tmp/body" ;;
    401) die "the token was refused (expired, revoked or mistyped): create a new one on the console and update KOROKU_TOKEN" ;;
    403) die "$path is not available with a read-only token ($(error_code))" ;;
    404) die "$path: not found ($(error_code))" ;;
    429) die "rate limited: wait a minute and try again" ;;
    *) die "$path: HTTP $code ($(error_code))" ;;
  esac
}

error_code() {
  sed -n 's/.*"code":"\([a-z_]*\)".*/\1/p' "$tmp/body" | head -n 1
}

# repo_key <url>: owner/name of a GitHub remote URL, lower-cased.
repo_key() {
  printf '%s\n' "$1" |
    sed -E -e 's#^(git\+)?(ssh://)?(https?://)?([^@/]+@)?github\.com[:/]##' -e 's#\.git$##' -e 's#/+$##' |
    tr '[:upper:]' '[:lower:]'
}

lines=""
app=""
all=0
service=""
ref=""
sha=""
cmd="status"
if [ $# -gt 0 ] && [ "${1#-}" = "$1" ]; then cmd="$1"; shift; fi
args=()
while [ $# -gt 0 ]; do
  case "$1" in
    --app) app="${2:?}"; shift 2 ;;
    --all) all=1; shift ;;
    --lines) lines="${2:?}"; shift 2 ;;
    --service) service="${2:?}"; shift 2 ;;
    --ref) ref="${2:?}"; shift 2 ;;
    --sha) sha="${2:?}"; shift 2 ;;
    -h | --help) sed -n '2,25p' "$0"; exit 0 ;;
    -*) die "unknown option $1" ;;
    *) args+=("$1"); shift ;;
  esac
done
case "$lines" in '' | *[!0-9]*) [ -z "$lines" ] || die "--lines takes a number" ;; esac

urlencode() {
  local s="$1" out="" c i
  for ((i = 0; i < ${#s}; i++)); do
    c="${s:i:1}"
    case "$c" in
      [A-Za-z0-9._~-]) out+="$c" ;;
      *) out+="$(printf '%%%02X' "'$c")" ;;
    esac
  done
  printf '%s' "$out"
}

show_apps() {
  if [ "$have_jq" = 1 ]; then
    jq -r '.apps[] | "\(.slug)\t\(.environment)\t\(.desired_state)\t\(if .repo_full_name != "" then .repo_full_name else .repo_url end)\tlatest: \(.latest_deployment.status // "none") #\(.latest_deployment.id // "-")\t\(.primary_url)"'
  else
    cat
  fi
}

show_deployments() {
  if [ "$have_jq" = 1 ]; then
    jq -r '.deployments[] | "#\(.id)\t\(.status)\t\(.ref)\t\(.commit_sha[0:12])\t\(.queued_at)\t\((.commit_message // "") | split("\n") | .[0] // "")\(if .error != "" then "\n    error: " + .error else "" end)"'
  else
    cat
  fi
}

deployment_log() {
  local id="$1" n="${2:-200}"
  if [ "$n" = 0 ]; then
    api "/api/deployments/$(urlencode "$id")/logs?format=text"
  else
    api "/api/deployments/$(urlencode "$id")/logs?format=text" | tail -n "$n"
  fi
}

# runtime_logs <slug> <service> <n>: the last n lines. The live stream
# starts with the newest lines (tail=n) and is cut after a few seconds.
runtime_logs() {
  local slug="$1" svc="$2" n="$3" query code
  query="tail=$n"
  [ -z "$svc" ] || query="$query&service=$(urlencode "$svc")"
  code=0
  : >"$tmp/sse"
  : >"$tmp/sse.code"
  printf 'header = "Authorization: Bearer %s"\n' "$KOROKU_TOKEN" |
    curl -sS -N -K - -H 'Accept: text/event-stream' --max-time 4 -o "$tmp/sse" -w '%{http_code}' \
      "$base/api/apps/$(urlencode "$slug")/logs?$query" >"$tmp/sse.code" 2>/dev/null || code=$?
  # 28 is curl's timeout: the stream stays open until we leave.
  if [ "$code" != 0 ] && [ "$code" != 28 ]; then
    echo "    (runtime logs unavailable: curl exit $code)"
    return
  fi
  case "$(cat "$tmp/sse.code")" in
    200) ;;
    *) cp "$tmp/sse" "$tmp/body"; echo "    (runtime logs unavailable: HTTP $(cat "$tmp/sse.code") $(error_code))"; return ;;
  esac
  if [ "$have_jq" = 1 ]; then
    sed -n 's/^data: //p' "$tmp/sse" | jq -r '"\(.time) \(.service) | \(.line)"'
  else
    sed -n 's/^data: //p' "$tmp/sse"
  fi
}

env_requirements() {
  local slug="$1" query=""
  if [ -n "$sha" ]; then
    query="?sha=$(urlencode "$sha")"
  elif [ -n "$ref" ]; then
    query="?ref=$(urlencode "$ref")"
  fi
  if [ "$have_jq" = 1 ]; then
    api "/api/apps/$(urlencode "$slug")/env/requirements$query" | jq -r '
      if .status != "ok" then "status: \(.status) \(.reason)"
      else
        "source: \(.source) \(.path) at \(.commit_sha[0:12])",
        (.variables[] | "\(if .set then "set    " elif .required then "MISSING" else "unset  " end) \(.key)\(if .required then " (required)" else "" end)\(if .description != "" then " - " + .description else "" end)")
      end'
  else
    api "/api/apps/$(urlencode "$slug")/env/requirements$query"
    echo
  fi
}

# matching_apps: slugs of the apps deployed from this repository.
matching_apps() {
  local remote key
  remote="$(git remote get-url origin 2>/dev/null)" || die "no origin remote here; pass --app <slug> or --all"
  key="$(repo_key "$remote")"
  if [ "$have_jq" = 1 ]; then
    api /api/apps | jq -r --arg key "$key" '
      def norm: ascii_downcase | sub("^(git\\+)?(ssh://)?(https?://)?([^@/]+@)?github\\.com[:/]"; "") | sub("\\.git$"; "") | sub("/+$"; "");
      .apps[] | select((.repo_full_name | ascii_downcase) == $key or ((.repo_url // "") != "" and (.repo_url | norm) == $key)) | .slug'
  else
    die "matching apps to this repository needs jq; pass --app <slug> (see: $0 apps)"
  fi
}

status_of() {
  local slug="$1" n="${lines:-80}"
  api "/api/apps/$(urlencode "$slug")" >"$tmp/app.json"
  echo "==> App $slug"
  if [ "$have_jq" = 0 ]; then
    cat "$tmp/app.json"
    echo
    echo "    (install jq for a summary; use: $0 log <id>, $0 runtime $slug)"
    return
  fi
  jq -r '
    "    environment: \(.environment), desired state: \(.desired_state), url: \(.primary_url)",
    "    repository: \(if .repo_full_name != "" then .repo_full_name else .repo_url end) (branch \(.default_branch)), compose: \(if .compose_path != "" then .compose_path else "(auto)" end), public: \(.web_service):\(.web_port), health: \(.healthcheck_path)",
    "    running: \(if .current_deployment then "#\(.current_deployment.id) \(.current_deployment.commit_sha[0:12]) \((.current_deployment.commit_message // "") | split("\n") | .[0] // "")" else "nothing" end)",
    "    latest:  \(if .latest_deployment then "#\(.latest_deployment.id) \(.latest_deployment.status) \(.latest_deployment.ref) \(.latest_deployment.commit_sha[0:12]) \((.latest_deployment.commit_message // "") | split("\n") | .[0] // "")" else "no deployment yet" end)",
    (if .latest_deployment and .latest_deployment.error != "" then "    error:   \(.latest_deployment.error)" else empty end)
  ' "$tmp/app.json"
  local status id sha_full
  status="$(jq -r '.latest_deployment.status // ""' "$tmp/app.json")"
  id="$(jq -r '.latest_deployment.id // ""' "$tmp/app.json")"
  sha_full="$(jq -r '.latest_deployment.commit_sha // ""' "$tmp/app.json")"
  case "$status" in
    failed | canceled)
      echo
      echo "==> Deployment #$id log (last $n lines; full: $0 log $id --lines 0)"
      deployment_log "$id" "$n" | sed 's/^/    /'
      echo
      echo "==> Services"
      api "/api/apps/$(urlencode "$slug")/services" |
        jq -r '.services[] | "    \(.name)\t\(.state)\t\(.status)\t\(if .health != "" then .health else "-" end)"'
      echo
      echo "==> Last runtime log lines"
      runtime_logs "$slug" "" 40 | sed 's/^/    /'
      echo
      echo "==> Environment variables the failed commit needs (keys only)"
      sha="$sha_full" env_requirements "$slug" | sed 's/^/    /'
      ;;
    queued | running)
      echo "    (in progress: run again later, or: $0 log $id)"
      ;;
  esac
}

case "$cmd" in
  apps)
    api /api/apps | show_apps
    ;;
  deployments)
    [ "${#args[@]}" = 1 ] || die "usage: deployments <slug>"
    api "/api/apps/$(urlencode "${args[0]}")/deployments?limit=10" | show_deployments
    ;;
  log)
    [ "${#args[@]}" = 1 ] || die "usage: log <deployment-id> [--lines N]"
    case "${args[0]}" in '' | *[!0-9]*) die "deployment id must be a number" ;; esac
    deployment_log "${args[0]}" "${lines:-0}"
    ;;
  runtime)
    [ "${#args[@]}" = 1 ] || die "usage: runtime <slug> [--service <name>] [--lines N]"
    runtime_logs "${args[0]}" "$service" "${lines:-100}"
    ;;
  env)
    [ "${#args[@]}" = 1 ] || die "usage: env <slug> [--ref <branch>|--sha <sha>]"
    env_requirements "${args[0]}"
    ;;
  status)
    slugs=()
    if [ -n "$app" ]; then
      slugs=("$app")
    elif [ "$all" = 1 ]; then
      [ "$have_jq" = 1 ] || { api /api/apps; exit 0; }
      found="$(api /api/apps | jq -r '.apps[].slug')"
      while IFS= read -r s; do [ -z "$s" ] || slugs+=("$s"); done <<<"$found"
    else
      found="$(matching_apps)"
      while IFS= read -r s; do [ -z "$s" ] || slugs+=("$s"); done <<<"$found"
      if [ "${#slugs[@]}" = 0 ]; then
        echo "No koroku app is deployed from $(git remote get-url origin). Apps on this koroku:"
        api /api/apps | show_apps | sed 's/^/    /'
        exit 1
      fi
    fi
    for s in "${slugs[@]}"; do
      status_of "$s"
      echo
    done
    ;;
  *)
    die "unknown command $cmd (status, apps, deployments, log, runtime, env)"
    ;;
esac
