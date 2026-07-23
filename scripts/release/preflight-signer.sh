#!/usr/bin/env bash
set -euo pipefail

trust=""; role=""
while (($#)); do
  case "$1" in
    --trust-config) trust="$2"; shift 2 ;;
    --role) role="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
[[ -f "$trust" && -n "$role" ]] || { echo "trust config and role are required" >&2; exit 2; }
command -v git >/dev/null
git config --get user.signingkey >/dev/null || { echo "hardware signing identity is not configured" >&2; exit 1; }
tmp_tag="agentbridge-signer-preflight-$$"
trap 'git tag -d "$tmp_tag" >/dev/null 2>&1 || true' EXIT
git tag -s -m "AgentBridge signer preflight" "$tmp_tag" HEAD
git verify-tag "$tmp_tag" >/dev/null
echo "signer preflight passed for $role"
