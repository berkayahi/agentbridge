#!/usr/bin/env bash
set -euo pipefail

bin_dir=$HOME/.local/bin
lib_dir=${AGENTBRIDGE_LIB_DIR:-$HOME/.local/lib/agentbridge}
unit_dir=$HOME/.config/systemd/user

systemctl --user disable --now agentbridge.service agentbridge-device-agent.service agentbridge-backup.timer 2>/dev/null || true
rm -f -- "$unit_dir/agentbridge.service" "$unit_dir/agentbridge-device-agent.service" "$unit_dir/agentbridge-backup.service" \
  "$unit_dir/agentbridge-backup.timer" "$bin_dir/agentbridge"
rm -rf -- "$lib_dir"
systemctl --user daemon-reload

printf 'Removed AgentBridge executables and units.\n'
printf 'Configuration, credentials, databases, backups, attachments, and worktrees were preserved.\n'
