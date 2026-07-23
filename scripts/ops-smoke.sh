#!/usr/bin/env bash
set -euo pipefail

binary="${AGENTBRIDGE_BINARY:-agentbridge}"
config="${AGENTBRIDGE_CONFIG:-}"
database="${DATABASE_PATH:-}"
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

if [[ -z "$config" && -f "$script_dir/../config.example.yaml" ]]; then
  config="$script_dir/../config.example.yaml"
fi

"$binary" version >/dev/null
if [[ -n "$config" && -f "$config" ]]; then
  "$binary" doctor --config "$config" >/dev/null
fi
if [[ -n "$database" && -f "$database" ]]; then
  "$binary" doctor --database "$database" --json >/dev/null
fi
printf 'AgentBridge bounded operations smoke passed.\n'
