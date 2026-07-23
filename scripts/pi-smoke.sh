#!/usr/bin/env bash
set -euo pipefail

PATH="$HOME/.local/bin:$PATH"
export PATH

config=${AGENTBRIDGE_CONFIG:-${XDG_CONFIG_HOME:-$HOME/.config}/agentbridge/config.yaml}
data_dir=${AGENTBRIDGE_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/agentbridge}
database=${DATABASE_PATH:-$data_dir/agentbridge.db}
agentbridge_bin=${AGENTBRIDGE_BINARY:-agentbridge}

"$agentbridge_bin" version
"$agentbridge_bin" doctor --config "$config"
if [[ -f "$database" ]]; then
  "$agentbridge_bin" doctor --database "$database" --json >/dev/null
fi
if command -v systemctl >/dev/null 2>&1; then
  systemctl --user is-active --quiet agentbridge.service
  systemctl --user is-active --quiet agentbridge-backup.timer
fi

printf 'AgentBridge Raspberry Pi smoke checks passed.\n'
