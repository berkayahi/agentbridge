#!/usr/bin/env bash
set -euo pipefail

PATH="$HOME/.local/bin:$PATH"
export PATH

config=${AGENTBRIDGE_CONFIG:-${XDG_CONFIG_HOME:-$HOME/.config}/agentbridge/config.yaml}
data_dir=${AGENTBRIDGE_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/agentbridge}
listen=${AGENTBRIDGE_LISTEN:-127.0.0.1:8787}

case "$listen" in
  127.0.0.1:*|localhost:*) ;;
  *) printf 'agentbridge: refusing smoke test against a non-loopback listener\n' >&2; exit 1 ;;
esac

for command in agentbridge git sqlite3 curl systemctl codex claude; do
  command -v "$command" >/dev/null 2>&1 || {
    printf 'agentbridge: missing required command: %s\n' "$command" >&2
    exit 1
  }
done

agentbridge version
git --version
sqlite3 --version
codex --version
claude --version
agentbridge doctor --config "$config"
systemctl --user is-active --quiet agentbridge.service
systemctl --user is-active --quiet agentbridge-backup.timer
curl --fail --silent --show-error --max-time 5 "http://$listen/healthz" >/dev/null

if [[ -f "$data_dir/agentbridge.db" ]]; then
  integrity=$(sqlite3 -batch "$data_dir/agentbridge.db" 'PRAGMA integrity_check;')
  [[ "$integrity" == "ok" ]] || {
    printf 'agentbridge: database integrity check failed: %s\n' "$integrity" >&2
    exit 1
  }
fi

if command -v tailscale >/dev/null 2>&1; then
  tailscale serve status >/dev/null
fi

printf 'AgentBridge Raspberry Pi smoke checks passed.\n'
