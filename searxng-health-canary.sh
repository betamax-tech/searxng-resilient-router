#!/usr/bin/env bash
# searxng-health-canary.sh — junk-detection health check for the
# searxng-resilient-router stack. Adapted from the investigation doc:
# /home/cmark/projects/openwebui-plugins/docs/investigations/2026-10-12-searxng-junk-results.md
#
# Exit 0 = healthy. Exit 1 = degraded (logged). Exit 2 = junk detected (logged
# + the offending URL). Both non-zero exits are logged to LOG and surfaced via
# `systemctl --user status searxng-health-canary.service` / journal, so a
# regression shows up the same day the timer catches it, not three months
# later the way the 2026-10-12 regression was discovered.
#
# Adaptation note: the doc's reference script used `set -euo pipefail`. Changed
# to `set -uo pipefail` (dropped -e) here deliberately: this script's whole job
# is to run ALL canaries and report every finding, not abort on the first
# transient curl/python hiccup mid-loop and silently skip the remaining
# canaries — that would itself be a silent-degradation trap.
set -uo pipefail

ROUTER="http://127.0.0.1:8899"
DIRECT_PROXIED="http://127.0.0.1:8891"
JUNK_HOSTS_RE='pornhub\.com|xvideos\.com|xnxx\.com|xhamster\.com|redtube\.com|youporn\.com|onlyfans\.com|chaturbate\.com|stripchat\.com|friv\.com|framesynthesis\.com'
LOG=/home/cmark/server/searxng-rotation/logs/health-canary.log
status=0

log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$1" | tee -a "$LOG"; }

mkdir -p "$(dirname "$LOG")"

# 1. Tier-2 (proxied) reachability — catches the P0 bug (500 on every request).
proxied_code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "${DIRECT_PROXIED}/search?q=test&format=json" || echo 000)
if [ "$proxied_code" != "200" ]; then
  log "DEGRADED tier2_proxied_unreachable status=$proxied_code"
  status=1
fi

# 2. Router-level canaries: a PH legal query, the exact bare-word canary that
#    broke on 2026-10-12, a PH legal docket query, and the pgvector technical
#    query named in the task.
declare -A CANARIES=(
  ["legal_cervantes"]="Cervantes v. Pangilinan A.M. No. MTJ-08-1709"
  ["legal_ra4726"]="RA 4726 Condominium Act Philippines"
  ["bareword_safesearch_canary"]="misconduct"
  ["technical_pgvector"]="pgvector HNSW index larger than RAM"
)

for name in "${!CANARIES[@]}"; do
  q="${CANARIES[$name]}"
  resp=$(curl -s --max-time 25 --get --data-urlencode "q=${q}" --data-urlencode "format=json" "${ROUTER}/search")
  n=$(echo "$resp" | python3 -c "import json,sys; d=json.load(sys.stdin); print(len(d.get('results',[])))" 2>/dev/null || echo -1)
  unresp=$(echo "$resp" | python3 -c "import json,sys; d=json.load(sys.stdin); print(','.join(u[0] for u in d.get('unresponsive_engines',[])))" 2>/dev/null || echo "")
  urls=$(echo "$resp" | python3 -c "import json,sys; d=json.load(sys.stdin); [print(r.get('url','')) for r in d.get('results',[])]" 2>/dev/null || echo "")

  if [ "$n" -le 0 ]; then
    log "DEGRADED canary=$name zero_results query='${q}'"
    status=1
  fi
  if echo "$urls" | grep -qiE "$JUNK_HOSTS_RE"; then
    hit=$(echo "$urls" | grep -iE "$JUNK_HOSTS_RE" | head -1)
    log "JUNK_DETECTED canary=$name query='${q}' url='${hit}'"
    status=2
  fi
  if [ -n "$unresp" ]; then
    n_unresp=$(echo "$unresp" | tr ',' '\n' | wc -l)
    if [ "$n_unresp" -ge 3 ]; then
      log "DEGRADED canary=$name engines_unresponsive=$unresp (>=3 major engines down)"
      status=1
    fi
  fi
done

if [ "$status" -eq 0 ]; then
  log "OK all canaries clean (tier2=200, zero junk hits, <3 engines down per canary)"
fi

exit $status
