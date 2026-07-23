#!/usr/bin/env bash
set -euo pipefail

tag=""; trust=""; role=""
while (($#)); do
  case "$1" in
    --tag) tag="$2"; shift 2 ;;
    --trust-config) trust="$2"; shift 2 ;;
    --role) role="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
[[ -n "$tag" && -f "$trust" && -n "$role" ]] || { echo "tag, trust config, and role are required" >&2; exit 2; }
case "$tag" in
  protocol/v1.0.0-rc.1) ;;
  protocol/v*) [[ "$role" != "protocol-rc-bootstrap" ]] || { echo "bootstrap signer cannot sign stable tags" >&2; exit 1; } ;;
  *) echo "tag is outside the protocol trust policy" >&2; exit 1 ;;
esac
git verify-tag "$tag" >/dev/null
signer="$(git for-each-ref "refs/tags/$tag" --format='%(signature:fingerprint)' 2>/dev/null || true)"
[[ -n "$signer" ]] || { echo "tag has no inspectable signer fingerprint" >&2; exit 1; }
allowed="$(awk -v role="$role" 'index($0, "\"" role "\"") { in_role=1; next } in_role && /\]/ { exit } in_role && /"[A-Fa-f0-9]{16,}"/ { gsub(/[^A-Fa-f0-9]/, ""); print }' "$trust")"
printf '%s\n' "$allowed" | rg -qi --fixed-strings "$signer" || { echo "tag signer is not trusted for role" >&2; exit 1; }
