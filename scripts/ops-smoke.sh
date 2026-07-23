#!/usr/bin/env bash
set -euo pipefail

binary="${AGENTBRIDGE_BINARY:-agentbridge}"
config="${AGENTBRIDGE_CONFIG:-}"
database="${DATABASE_PATH:-}"

"$binary" version >/dev/null
if [[ -n "$config" ]]; then
  "$binary" doctor --config "$config" >/dev/null
fi
if [[ -n "$database" && -f "$database" ]]; then
  "$binary" doctor --database "$database" --json >/dev/null
fi
printf 'AgentBridge bounded operations smoke passed.\n'
