#!/usr/bin/env bash
set -euo pipefail

umask 077

if [[ $# -lt 1 || $# -gt 2 ]]; then
  printf 'usage: %s <backup-database-or-directory> [work-directory]\n' "${0##*/}" >&2
  exit 2
fi

work_dir=${2:-${TMPDIR:-/tmp}/agentbridge-restore-check}
agentbridge_bin=${AGENTBRIDGE_BINARY:-agentbridge}
exec "$agentbridge_bin" restore-check --backup "$1" --work-dir "$work_dir"
