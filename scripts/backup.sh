#!/usr/bin/env bash
set -euo pipefail

umask 077

data_dir=${AGENTBRIDGE_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/agentbridge}
database=${DATABASE_PATH:-$data_dir/agentbridge.db}
backup_dir=${BACKUP_DIR:-$data_dir/backups}
agentbridge_bin=${AGENTBRIDGE_BINARY:-agentbridge}

if [[ "$agentbridge_bin" == */* ]]; then
  [[ -x "$agentbridge_bin" ]] || { printf 'agentbridge: binary is not executable: %s\n' "$agentbridge_bin" >&2; exit 1; }
else
  command -v "$agentbridge_bin" >/dev/null 2>&1 || { printf 'agentbridge: binary is not on PATH: %s\n' "$agentbridge_bin" >&2; exit 1; }
fi
[[ -f "$database" ]] || {
  printf 'agentbridge: database not found\n' >&2
  exit 1
}

exec "$agentbridge_bin" backup --database "$database" --output "$backup_dir"
