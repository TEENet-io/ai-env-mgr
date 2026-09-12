#!/usr/bin/env bash
set -euo pipefail
PROFILE=${PROFILE:-sls-bootstrap}; PRODUCT=${PRODUCT:-sls}; REGION=ap-southeast-1; PROJECT=windows-control-logs
HERE=$(cd "$(dirname "$0")" && pwd)
A() { aliyun "$PRODUCT" "$@" --profile "$PROFILE" --region "$REGION" --force; }
# input_file takes exactly ONE path per config (Logtail 2.1 rejects a list),
# hence one config per file on the console host.
apply() { # name file group
  A GetLogtailPipelineConfig --project "$PROJECT" --configName "$1" >/dev/null 2>&1 \
    && A UpdateLogtailPipelineConfig --project "$PROJECT" --configName "$1" --body "$(cat "$HERE/$2")" \
    || A CreateLogtailPipelineConfig --project "$PROJECT" --body "$(cat "$HERE/$2")"
  A ApplyConfigToMachineGroup --project "$PROJECT" --machineGroup "$3" --configName "$1"
}
apply console-ops     pipeline-console-ops.json     console-host
apply console-probe   pipeline-console-probe.json   console-host
apply console-audit   pipeline-console-audit.json   console-host
apply gateway-audit   pipeline-gateway-audit.json   gateway-host
apply gateway-stdout  pipeline-gateway-stdout.json  gateway-host
