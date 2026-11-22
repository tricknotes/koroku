#!/usr/bin/env bash
# Measure the memory (and optionally the latency and throughput) of a
# running Compose project the way koroku shows it: the sum of `docker stats`
# over the project's containers, after a load, once the app has settled.
#
# Start the stack first, for example with the prepare-app Skill's
# rehearse.sh --keep (it prints the project name), then:
#
#   measure.sh --project <name> --path / --path /about
#
# Usage: measure.sh --project <name> [options]
#   --project <name>     Compose project to measure (required)
#   --url <base>         base URL of the public service
#                        (default: http://127.0.0.1:28080)
#   --host <name>        Host header to send (default: the URL's host). Use
#                        the production host name when the app only answers
#                        its own host (Rails host authorization, canonical
#                        host redirects)
#   --path <path>        page to load; repeatable (default: /)
#   --concurrency <n>    parallel requests during the load (default: 30)
#   --repeat <n>         requests per page and round (default: 30)
#   --rounds <n>         load rounds (default: 3)
#   --settle <seconds>   wait after the load before reading memory (default: 5)
#   --bench              also measure latency (after a warm-up of 300
#                        requests per page: 200 sequential requests per
#                        page, mean/p50/p95) and throughput (3 clients for
#                        20 s over all pages)
#
# Every request carries X-Forwarded-Proto: https, as koroku's proxy sends
# it, so apps that force HTTPS answer instead of redirecting.
set -euo pipefail

project=""
url="http://127.0.0.1:28080"
host=""
paths=()
concurrency=30
repeat=30
rounds=3
settle=5
bench=0

die() { echo "measure: $*" >&2; exit 2; }

while [ $# -gt 0 ]; do
  case "$1" in
    --project) project="${2:?}"; shift 2 ;;
    --url) url="${2:?}"; shift 2 ;;
    --host) host="${2:?}"; shift 2 ;;
    --path) paths+=("${2:?}"); shift 2 ;;
    --concurrency) concurrency="${2:?}"; shift 2 ;;
    --repeat) repeat="${2:?}"; shift 2 ;;
    --rounds) rounds="${2:?}"; shift 2 ;;
    --settle) settle="${2:?}"; shift 2 ;;
    --bench) bench=1; shift ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

[ -n "$project" ] || die "--project is required"
case "$concurrency$repeat$rounds$settle" in *[!0-9]*) die "numbers expected" ;; esac
[ ${#paths[@]} -gt 0 ] || paths=(/)
for p in "${paths[@]}"; do case "$p" in /*) ;; *) die "--path must start with /: $p" ;; esac; done
command -v docker >/dev/null || die "docker is not installed"
command -v curl >/dev/null || die "curl is not installed"

url="${url%/}"
if [ -z "$host" ]; then
  host="$(printf '%s\n' "$url" | sed -E 's#^[a-z]+://([^/:]+).*#\1#')"
fi
headers=(-H "Host: $host" -H "X-Forwarded-Proto: https")

mapfile -t containers < <(docker ps -q --filter "label=com.docker.compose.project=$project")
[ ${#containers[@]} -gt 0 ] || die "no running containers in project $project"

# Status codes of one request per page, so a load of redirects or errors is
# noticed before its numbers are trusted.
echo "==> Pages"
for p in "${paths[@]}"; do
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 30 "${headers[@]}" "$url$p" || true)"
  printf '    %s %s\n' "$code" "$p"
done

echo "==> Load: ${#paths[@]} page(s) x $repeat x $rounds round(s), $concurrency in parallel"
total=$(( ${#paths[@]} * repeat ))
for round in $(seq "$rounds"); do
  for _ in $(seq "$repeat"); do printf '%s\n' "${paths[@]}"; done |
    xargs -P "$concurrency" -I{} curl -s -o /dev/null --max-time 60 "${headers[@]}" "$url{}" || true
  echo "    round $round: $total requests"
done

sleep "$settle"

# docker stats reports memory the way koroku's resources tab and memory
# history do (cgroup usage without inactive file cache).
echo "==> Memory after the load (docker stats)"
docker stats --no-stream --format '{{.Name}}|{{.MemUsage}}' "${containers[@]}" |
  awk -F'|' '
    function mib(v,   n, u) {
      n = v + 0; u = v; sub(/^[0-9.]+/, "", u)
      if (u == "GiB") return n * 1024
      if (u == "MiB") return n
      if (u == "KiB" || u == "kB") return n / 1024
      if (u == "B") return n / 1048576
      if (u == "GB") return n * 1000000000 / 1048576
      if (u == "MB") return n * 1000000 / 1048576
      return n
    }
    { split($2, a, " / "); m = mib(a[1]); sum += m; printf "    %-40s %8.1f MiB\n", $1, m }
    END { printf "    %-40s %8.1f MiB\n", "total", sum }' | sort -k1,1 | awk '/ total / { t = $0; next } { print } END { print t }'

[ "$bench" -eq 1 ] || exit 0

echo "==> Latency (after 300 warm-up requests per page; 200 sequential per page)"
for p in "${paths[@]}"; do
  for _ in $(seq 300); do curl -s -o /dev/null --max-time 60 "${headers[@]}" "$url$p" || true; done
  for _ in $(seq 200); do curl -s -o /dev/null -w '%{time_total}\n' --max-time 60 "${headers[@]}" "$url$p" || true; done |
    sort -n | awk -v p="$p" '
      function at(q,   i) { i = int(NR * q); return a[i < 1 ? 1 : i] * 1000 }
      { a[NR] = $1; s += $1 }
      END { if (NR) printf "    %-32s mean %7.1f ms  p50 %7.1f ms  p95 %7.1f ms\n", p, s / NR * 1000, at(0.5), at(0.95) }'
done

echo "==> Throughput (3 clients for 20 s over all pages)"
end=$(( $(date +%s) + 20 ))
count_file="$(mktemp)"
for _ in 1 2 3; do
  (
    n=0
    while [ "$(date +%s)" -lt "$end" ]; do
      for p in "${paths[@]}"; do
        curl -s -o /dev/null --max-time 60 "${headers[@]}" "$url$p" || true
        n=$(( n + 1 ))
      done
    done
    echo "$n" >> "$count_file"
  ) &
done
wait
awk '{ s += $1 } END { printf "    %.1f requests/s\n", s / 20 }' "$count_file"
rm -f "$count_file"
