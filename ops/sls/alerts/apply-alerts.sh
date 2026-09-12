#!/usr/bin/env bash
set -euo pipefail
PROFILE=${PROFILE:-sls-bootstrap}; PRODUCT=${PRODUCT:-sls}; REGION=ap-southeast-1; PROJECT=windows-control-logs
HERE=$(cd "$(dirname "$0")" && pwd)
A() { aliyun "$PRODUCT" "$@" --profile "$PROFILE" --region "$REGION" --force; }
for f in gateway-down upstream-error-rate budget-80 collection-stalled; do
  name=$(python3 -c "import json;print(json.load(open('$HERE/$f.json'))['name'])")
  A GetAlert --project "$PROJECT" --alertName "$name" >/dev/null 2>&1 \
    && A UpdateAlert --project "$PROJECT" --alertName "$name" --body "$(cat "$HERE/$f.json")" \
    || A CreateAlert --project "$PROJECT" --body "$(cat "$HERE/$f.json")"
done
