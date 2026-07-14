#!/usr/bin/env bash
set -euo pipefail

umask 077

data_dir=${AGENTBRIDGE_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/agentbridge}
DATABASE_PATH=${DATABASE_PATH:-$data_dir/agentbridge.db}
BACKUP_DIR=${BACKUP_DIR:-$data_dir/backups}
ATTACHMENT_ROOT=${ATTACHMENT_ROOT:-$data_dir/attachments}
WORKTREE_ROOT=${WORKTREE_ROOT:-$data_dir/worktrees}
PINNED_TASKS_FILE=${PINNED_TASKS_FILE:-$data_dir/pinned-task-ids}
EVENT_RETENTION_DAYS=${EVENT_RETENTION_DAYS:-30}
ARTIFACT_RETENTION_DAYS=${ARTIFACT_RETENTION_DAYS:-7}
BACKUP_RETENTION_DAYS=${BACKUP_RETENTION_DAYS:-14}

fail() {
  printf 'agentbridge: %s\n' "$1" >&2
  exit 1
}

for command in sqlite3 flock realpath; do
  command -v "$command" >/dev/null 2>&1 || fail "$command is required"
done
for value in "$EVENT_RETENTION_DAYS" "$ARTIFACT_RETENTION_DAYS" "$BACKUP_RETENTION_DAYS"; do
  [[ "$value" =~ ^[1-9][0-9]*$ ]] || fail "retention values must be positive integers"
done
[[ -f "$DATABASE_PATH" ]] || fail "database not found"
case "$DATABASE_PATH$BACKUP_DIR" in
  *"'"*|*$'\n'*) fail "database and backup paths cannot contain quotes or newlines" ;;
esac

mkdir -p -- "$BACKUP_DIR" "$ATTACHMENT_ROOT" "$WORKTREE_ROOT"
lock_file=$data_dir/backup.lock
exec 9>"$lock_file"
flock -n 9 || fail "another backup job is active"

timestamp=$(date -u +%Y%m%dT%H%M%SZ)
temporary=$(mktemp "$BACKUP_DIR/.agentbridge-$timestamp.XXXXXX.db")
destination=$BACKUP_DIR/agentbridge-$timestamp.db
trap 'rm -f -- "$temporary" "$temporary-wal" "$temporary-shm"' EXIT

# SQLite's online backup API produces a consistent snapshot of a live WAL database.
sqlite3 "$DATABASE_PATH" ".timeout 10000" ".backup '$temporary'"
integrity=$(sqlite3 -batch "$temporary" 'PRAGMA integrity_check;')
[[ "$integrity" == "ok" ]] || fail "backup integrity check failed: $integrity"
chmod 0600 "$temporary"
mv -- "$temporary" "$destination"

pinned_predicate='1=1'
if [[ -f "$PINNED_TASKS_FILE" ]]; then
  pinned_ids=()
  while IFS= read -r task_id || [[ -n "$task_id" ]]; do
    [[ -z "$task_id" || "$task_id" == \#* ]] && continue
    [[ "$task_id" =~ ^[A-Za-z0-9._:-]+$ ]] || fail "invalid pinned task ID"
    pinned_ids+=("'$task_id'")
  done < "$PINNED_TASKS_FILE"
  if ((${#pinned_ids[@]} > 0)); then
    pinned_list=$(IFS=,; printf '%s' "${pinned_ids[*]}")
    pinned_predicate="t.id NOT IN ($pinned_list)"
  fi
fi

inactive_predicate="t.state NOT IN ('queued','preparing','running','awaiting_approval','awaiting_auth','verifying','committing','pushing','paused')"
artifact_cutoff="datetime('now','-$ARTIFACT_RETENTION_DAYS days')"

delete_managed_paths() {
  local root=$1
  local query=$2
  local canonical_root path candidate
  canonical_root=$(realpath -m -- "$root")
  while IFS= read -r path; do
    [[ -z "$path" ]] && continue
    if [[ "$path" = /* ]]; then
      candidate=$(realpath -m -- "$path")
    else
      candidate=$(realpath -m -- "$root/$path")
    fi
    case "$candidate" in
      "$canonical_root"/*) rm -rf -- "$candidate" ;;
      *) fail "refusing to remove a path outside its managed root" ;;
    esac
  done < <(sqlite3 -batch -noheader "$DATABASE_PATH" "$query")
}

delete_managed_paths "$ATTACHMENT_ROOT" "
  SELECT a.storage_path FROM attachments a JOIN tasks t ON t.id=a.task_id
  WHERE datetime(a.created_at) < $artifact_cutoff AND $inactive_predicate AND $pinned_predicate;"
delete_managed_paths "$WORKTREE_ROOT" "
  SELECT t.worktree_path FROM tasks t
  WHERE t.state IN ('failed','canceled') AND datetime(t.updated_at) < $artifact_cutoff AND $pinned_predicate;"

sqlite3 -batch "$DATABASE_PATH" "
  PRAGMA busy_timeout=10000;
  BEGIN IMMEDIATE;
  DELETE FROM task_events
    WHERE datetime(created_at) < datetime('now','-$EVENT_RETENTION_DAYS days')
      AND task_id IN (SELECT t.id FROM tasks t WHERE $inactive_predicate AND $pinned_predicate);
  DELETE FROM attachments
    WHERE datetime(created_at) < $artifact_cutoff
      AND task_id IN (SELECT t.id FROM tasks t WHERE $inactive_predicate AND $pinned_predicate);
  UPDATE tasks AS t SET worktree_path=''
    WHERE t.state IN ('failed','canceled') AND datetime(t.updated_at) < $artifact_cutoff
      AND $pinned_predicate;
  COMMIT;"

find "$BACKUP_DIR" -type f -name 'agentbridge-*.db' -mtime "+$BACKUP_RETENTION_DAYS" -delete
printf 'Created verified backup: %s\n' "$destination"
