#!/usr/bin/env bash
set -euo pipefail

policy=""
repository="$(git rev-parse --show-toplevel)"
print_url=0
while (($#)); do
  case "$1" in
    --policy) policy="$2"; shift 2 ;;
    --repository) repository="$2"; shift 2 ;;
    --print-url) print_url=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
[[ -n "$policy" && -f "$policy" ]] || { echo "release policy is required" >&2; exit 2; }
canonical="$(sed -n 's/.*"canonical_remote"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$policy")"
[[ "$canonical" == "github.com/berkayahi/agentbridge" ]] || { echo "invalid canonical remote policy" >&2; exit 1; }
remote="$(git -C "$repository" config --get remote.origin.url || true)"
normalized="$(printf '%s' "$remote" | sed -E 's#^[^@]+@([^:]+):#\1/#; s#^[a-zA-Z]+://([^/]+)/#\1/#; s#/$##; s#\.git$##')"
[[ "$normalized" == "$canonical" ]] || { echo "origin is not the canonical repository" >&2; exit 1; }
if ((print_url)); then printf 'https://%s.git\n' "$canonical"; fi
