#!/usr/bin/env bash
# Put one deployment's agent.exe where that console's version library expects
# it, so the 10 MB binary never has to pass through the console host.
#
#   upload-agent.sh <deployment> <file> required|optional
#
# The key comes from OSS_CI_ACCESS_KEY_ID / _SECRET / OSS_CI_BUCKET /
# OSS_CI_ENDPOINT. "optional" skips with a warning when none of them is set
# (a deployment whose secrets are not in the repository yet); one set and
# another missing is always an error. An existing object is never overwritten
# -- a version names one set of bytes -- so a re-cut of the same tag fails on
# purpose.
set -euo pipefail
name=$1 file=$2 mode=$3
vars=(OSS_CI_ACCESS_KEY_ID OSS_CI_ACCESS_KEY_SECRET OSS_CI_BUCKET OSS_CI_ENDPOINT)
set_count=0
for v in "${vars[@]}"; do [ -n "${!v:-}" ] && set_count=$((set_count + 1)); done
if [ "$set_count" -eq 0 ] && [ "$mode" = optional ]; then
  echo "::warning::${name}: no upload secrets set; ${file} is attached to the release but not uploaded"
  exit 0
fi
for v in "${vars[@]}"; do
  [ -n "${!v:-}" ] || { echo "::error::${name}: $v is empty; ${file} cannot be uploaded"; exit 1; }
done
# The key may carry the in-region endpoint (…-internal.aliyuncs.com), which a
# GitHub runner cannot reach: ossutil would retry for hours.
OSS_CI_ENDPOINT="${OSS_CI_ENDPOINT/-internal./.}"
VER="${GITHUB_REF_NAME#v}"
KEY="agent_workdir/_agent/${VER}/agent.exe"
# Downloaded once per job: the second deployment's upload reuses it.
OSSUTIL=/tmp/ossutil/ossutil64
if [ ! -x "$OSSUTIL" ]; then
  curl -fsSL https://gosspublic.alicdn.com/ossutil/1.7.19/ossutil-v1.7.19-linux-amd64.zip -o /tmp/ossutil.zip
  unzip -q -o /tmp/ossutil.zip -d /tmp/ossutil.d
  mkdir -p /tmp/ossutil
  install -m 755 "$(find /tmp/ossutil.d -name 'ossutil64' -type f | head -1)" "$OSSUTIL"
fi
CFG=(-e "${OSS_CI_ENDPOINT}" -i "${OSS_CI_ACCESS_KEY_ID}" -k "${OSS_CI_ACCESS_KEY_SECRET}")
if timeout 120 "$OSSUTIL" "${CFG[@]}" stat "oss://${OSS_CI_BUCKET}/${KEY}" >/dev/null 2>&1; then
  echo "::error::${name}: ${KEY} already exists; a version names one set of bytes -- cut a new version"
  exit 1
fi
timeout 900 "$OSSUTIL" "${CFG[@]}" cp "${file}" "oss://${OSS_CI_BUCKET}/${KEY}"
echo "${name}: uploaded ${KEY} sha256 $(sha256sum "${file}" | cut -d' ' -f1)"
