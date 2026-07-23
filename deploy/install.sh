#!/usr/bin/env bash
set -euo pipefail

umask 077

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repository_root=$(cd -- "$script_dir/.." && pwd)
binary_source=${AGENTBRIDGE_BINARY:-$repository_root/bin/agentbridge}
config_source=${AGENTBRIDGE_CONFIG:-$repository_root/config.example.yaml}

bin_dir=$HOME/.local/bin
lib_dir=${AGENTBRIDGE_LIB_DIR:-$HOME/.local/lib/agentbridge}
data_dir=$HOME/.local/share/agentbridge
cache_dir=$HOME/.cache/agentbridge
config_dir=$HOME/.config/agentbridge
unit_dir=$HOME/.config/systemd/user

if [[ ! -x "$binary_source" ]]; then
  printf 'agentbridge: executable not found: %s\n' "$binary_source" >&2
  printf 'Build it with "make build" or set AGENTBRIDGE_BINARY.\n' >&2
  exit 1
fi

install -d -m 0700 "$bin_dir" "$lib_dir/scripts" "$data_dir" "$cache_dir" "$data_dir/backups" \
  "$data_dir/attachments" "$data_dir/worktrees" "$config_dir/credentials" "$unit_dir"
install -m 0755 "$binary_source" "$bin_dir/agentbridge"
install -m 0755 "$repository_root/scripts/backup.sh" "$lib_dir/scripts/backup.sh"
install -m 0755 "$repository_root/scripts/restore-check.sh" "$lib_dir/scripts/restore-check.sh"
install -m 0755 "$repository_root/scripts/ops-smoke.sh" "$lib_dir/scripts/ops-smoke.sh"
install -m 0755 "$repository_root/scripts/pi-smoke.sh" "$lib_dir/scripts/pi-smoke.sh"
install -m 0644 "$script_dir/systemd/agentbridge.service" "$unit_dir/agentbridge.service"
install -m 0644 "$script_dir/systemd/agentbridge-backup.service" "$unit_dir/agentbridge-backup.service"
install -m 0644 "$script_dir/systemd/agentbridge-backup.timer" "$unit_dir/agentbridge-backup.timer"

for credential in telegram_bot_token; do
  if [[ ! -e "$config_dir/credentials/$credential" ]]; then
    install -m 0600 /dev/null "$config_dir/credentials/$credential"
  fi
done

if [[ ! -e "$config_dir/config.yaml" ]]; then
  install -m 0600 "$config_source" "$config_dir/config.yaml"
  printf 'Created %s; replace every placeholder before starting the service.\n' "$config_dir/config.yaml"
fi

systemctl --user daemon-reload
printf 'Installed AgentBridge. Run doctor before enabling the service:\n'
printf '  %q doctor --config %q\n' "$bin_dir/agentbridge" "$config_dir/config.yaml"
printf '  systemctl --user enable --now agentbridge.service agentbridge-backup.timer\n'
