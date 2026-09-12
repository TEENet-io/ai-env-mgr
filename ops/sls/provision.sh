#!/usr/bin/env bash
# Creates the SLS project, both logstores, their indexes, the two machine
# groups and the two RAM identities. Safe to re-run: every create is guarded
# by a get. Run with: PROFILE=sls-bootstrap ops/sls/provision.sh
set -euo pipefail
PROFILE=${PROFILE:-sls-bootstrap}
PRODUCT=${PRODUCT:-sls}   # needs aliyun CLI >= 3.5.0 (3.0.x has no sls product)
REGION=ap-southeast-1
PROJECT=windows-control-logs
HERE=$(cd "$(dirname "$0")" && pwd)
A() { aliyun "$PRODUCT" "$@" --profile "$PROFILE" --region "$REGION" --force; }

echo "== project"
A GetProject --project "$PROJECT" >/dev/null 2>&1 || \
  A CreateProject --body "{\"projectName\":\"$PROJECT\",\"description\":\"ai-env-mgr unified logs (phase 1)\"}"

echo "== logstores"
A GetLogStore --project "$PROJECT" --logstore ops >/dev/null 2>&1 || \
  A CreateLogStore --project "$PROJECT" --body '{"logstoreName":"ops","ttl":30,"shardCount":2,"autoSplit":true,"maxSplitShard":8}'
A GetLogStore --project "$PROJECT" --logstore audit >/dev/null 2>&1 || \
  A CreateLogStore --project "$PROJECT" --body '{"logstoreName":"audit","ttl":365,"hot_ttl":30,"shardCount":2,"autoSplit":true,"maxSplitShard":8}'

echo "== indexes"
A GetIndex --project "$PROJECT" --logstore ops >/dev/null 2>&1 || \
  A CreateIndex --project "$PROJECT" --logstore ops --body "file://$HERE/index-ops.json"
A GetIndex --project "$PROJECT" --logstore audit >/dev/null 2>&1 || \
  A CreateIndex --project "$PROJECT" --logstore audit --body "file://$HERE/index-audit.json"

echo "== machine groups (custom identifier; hosts declare it in /etc/ilogtail/user_defined_id)"
for g in console-host:wc-console gateway-host:wc-gateway; do
  name=${g%%:*}; id=${g##*:}
  A GetMachineGroup --project "$PROJECT" --machineGroup "$name" >/dev/null 2>&1 || \
    A CreateMachineGroup --project "$PROJECT" --body "{\"groupName\":\"$name\",\"machineIdentifyType\":\"userdefined\",\"machineList\":[\"$id\"]}"
done

echo "== RAM identities"
R() { aliyun ram "$@" --profile "$PROFILE" --region "$REGION" --force; }
for spec in wc-logs-writer:policy-writer.json wc-logs-reader:policy-reader.json; do
  u=${spec%%:*}; p=${spec##*:}; pol=${u}-policy
  R GetUser --UserName "$u" >/dev/null 2>&1 || R CreateUser --UserName "$u" --DisplayName "$u"
  R GetPolicy --PolicyType Custom --PolicyName "$pol" >/dev/null 2>&1 || \
    R CreatePolicy --PolicyName "$pol" --PolicyDocument "$(cat "$HERE/$p")"
  R AttachPolicyToUser --PolicyType Custom --PolicyName "$pol" --UserName "$u" >/dev/null 2>&1 || true
done
echo "create AKs by hand (they print once):"
echo "  aliyun ram CreateAccessKey --UserName wc-logs-writer --profile $PROFILE"
echo "  aliyun ram CreateAccessKey --UserName wc-logs-reader --profile $PROFILE"
echo "done"
