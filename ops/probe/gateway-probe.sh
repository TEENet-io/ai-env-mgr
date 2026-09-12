#!/usr/bin/env bash
# Independent gateway health probe for the unified log (phase 1).
#
# One JSON line per run into probe.jsonl, shipped to SLS "ops" by Logtail.
# It reads only the public health endpoint and carries no key, and it runs
# from a systemd timer on the console host -- so a gateway outage is seen
# by something that does not depend on the gateway's own logging chain.
set -u
OUT=${PROBE_LOG:-/var/log/ai-env-mgr/probe.jsonl}
URL=${GATEWAY_HEALTH_URL:-https://litellm.teenet.app/health/liveliness}
start=$(date +%s%3N)
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$URL" || echo 000)
end=$(date +%s%3N)
ok=false; level=error; msg="gateway probe failed ($code)"
if [ "$code" = "200" ]; then ok=true; level=info; msg="gateway probe ok"; fi
# $((10#$code)) turns "000" into 0 and keeps "200" as 200: no leading zeros in JSON.
printf '{"schema_version":1,"event_id":"%s","event_type":"platform_event","occurred_at":"%s","module":"probe","source_id":"%s","level":"%s","message":"%s","target":"gateway","url":"%s","status":%s,"ok":%s,"latency_ms":%s}\n' \
  "$(cat /proc/sys/kernel/random/uuid)" "$(date -u +%Y-%m-%dT%H:%M:%S.000Z)" "probe@$(hostname)" "$level" "$msg" "$URL" "$((10#$code))" "$ok" "$((end-start))" >> "$OUT"
