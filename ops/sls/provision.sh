#!/usr/bin/env bash
# Creates the SLS project, both logstores, their indexes, the two machine
# groups and the two RAM identities. Safe to re-run: every create is guarded
# by a get. Run with: PROFILE=sls-bootstrap ops/sls/provision.sh
set -euo pipefail
PROFILE=${PROFILE:-sls-bootstrap}
PRODUCT=${PRODUCT:-sls}   # needs aliyun CLI >= 3.5.0 (3.0.x has no sls product); --body file:// is NOT honoured, bodies are inlined
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
# Indexes converge: an existing index is updated from the file, so editing
# index-*.json and re-running applies the change.
for ls in ops audit; do
  if A GetIndex --project "$PROJECT" --logstore "$ls" >/dev/null 2>&1; then
    A UpdateIndex --project "$PROJECT" --logstore "$ls" --body "$(cat "$HERE/index-$ls.json")"
  else
    A CreateIndex --project "$PROJECT" --logstore "$ls" --body "$(cat "$HERE/index-$ls.json")"
  fi
done

echo "== machine groups (custom identifier; hosts declare it in /etc/ilogtail/user_defined_id)"
for g in console-host:wc-console gateway-host:wc-gateway; do
  name=${g%%:*}; id=${g##*:}
  A GetMachineGroup --project "$PROJECT" --machineGroup "$name" >/dev/null 2>&1 || \
    A CreateMachineGroup --project "$PROJECT" --body "{\"groupName\":\"$name\",\"machineIdentifyType\":\"userdefined\",\"machineList\":[\"$id\"]}"
done

if [ "${SKIP_RAM:-0}" = "1" ]; then
  echo "== RAM identities skipped (SKIP_RAM=1): the bootstrap AK has no RAM rights; create wc-logs-writer/reader by hand from policy-*.json"
  exit 0
fi
echo "== RAM identities"
R() { aliyun ram "$@" --profile "$PROFILE" --region "$REGION" --force; }
for spec in wc-logs-writer:policy-writer.json wc-logs-reader:policy-reader.json; do
  u=${spec%%:*}; p=${spec##*:}; pol=${u}-policy
  R GetUser --UserName "$u" >/dev/null 2>&1 || R CreateUser --UserName "$u" --DisplayName "$u"
  # RAM policies do NOT converge: an existing policy keeps its old document.
  # After editing policy-*.json run CreatePolicyVersion --SetAsDefault true.
  R GetPolicy --PolicyType Custom --PolicyName "$pol" >/dev/null 2>&1 || \
    R CreatePolicy --PolicyName "$pol" --PolicyDocument "$(cat "$HERE/$p")"
  # Attach only when missing: a swallowed attach failure would leave an
  # identity that looks provisioned but cannot write.
  if ! R ListPoliciesForUser --UserName "$u" | grep -q "\"PolicyName\": *\"$pol\""; then
    R AttachPolicyToUser --PolicyType Custom --PolicyName "$pol" --UserName "$u"
  fi
done
echo "create AKs by hand (they print once):"
echo "  aliyun ram CreateAccessKey --UserName wc-logs-writer --profile $PROFILE"
echo "  aliyun ram CreateAccessKey --UserName wc-logs-reader --profile $PROFILE"
echo "done"
