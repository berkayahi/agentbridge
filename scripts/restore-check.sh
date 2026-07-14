#!/usr/bin/env bash
set -euo pipefail

umask 077

if [[ $# -ne 1 ]]; then
  printf 'usage: %s <backup-database>\n' "${0##*/}" >&2
  exit 2
fi

backup=$1
if [[ ! -f "$backup" ]]; then
  printf 'agentbridge: backup does not exist: %s\n' "$backup" >&2
  exit 1
fi
if ! command -v sqlite3 >/dev/null 2>&1; then
  printf 'agentbridge: sqlite3 is required\n' >&2
  exit 1
fi

scratch=$(mktemp "${TMPDIR:-/tmp}/agentbridge-restore-check.XXXXXX.db")
trap 'rm -f -- "$scratch" "$scratch-wal" "$scratch-shm"' EXIT

sqlite3 "$backup" ".timeout 10000" ".backup '$scratch'"
result=$(sqlite3 -batch "$scratch" 'PRAGMA integrity_check;')
if [[ "$result" != "ok" ]]; then
  printf 'agentbridge: restore integrity check failed: %s\n' "$result" >&2
  exit 1
fi

tables=$(sqlite3 -batch "$scratch" "SELECT count(*) FROM sqlite_schema WHERE type='table' AND name IN ('tasks','task_events','attachments');")
if [[ "$tables" != "3" ]]; then
  printf 'agentbridge: backup is missing required tables\n' >&2
  exit 1
fi

printf 'Restore check passed: %s\n' "$backup"
