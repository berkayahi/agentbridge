#!/usr/bin/env bash
set -euo pipefail

if [[ ${RUN_PI_SMOKE:-0} != "1" ]]; then
  printf 'not_executed: Raspberry Pi/systemd smoke requires RUN_PI_SMOKE=1\n' >&2
  exit 3
fi

if [[ "$(uname -s)" != "Linux" || "$(uname -m)" != "aarch64" ]]; then
  printf 'not_executed: controlled ARM64 Linux hardware is required\n' >&2
  exit 3
fi

PATH="$HOME/.local/bin:$PATH"
export PATH

config=${AGENTBRIDGE_CONFIG:-${XDG_CONFIG_HOME:-$HOME/.config}/agentbridge/config.yaml}
data_dir=${AGENTBRIDGE_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/agentbridge}
database=${DATABASE_PATH:-$data_dir/agentbridge.db}
agentbridge_bin=${AGENTBRIDGE_BINARY:-agentbridge}

if [[ ! -x "$agentbridge_bin" && "$(command -v "$agentbridge_bin" || true)" == "" ]]; then
  printf 'not_executed: candidate AgentBridge binary is unavailable\n' >&2
  exit 3
fi

if [[ -z "${AGENTBRIDGE_CANDIDATE_MANIFEST:-}" || ! -f "${AGENTBRIDGE_CANDIDATE_MANIFEST:-}" ]]; then
  printf 'not_executed: AGENTBRIDGE_CANDIDATE_MANIFEST must identify the immutable candidate manifest\n' >&2
  exit 3
fi
if [[ -z "${AGENTBRIDGE_CANDIDATE_SHA256:-}" || -z "${AGENTBRIDGE_MANIFEST_SHA256:-}" || -z "${AGENTBRIDGE_PI_JOB_ID:-}" ]]; then
  printf 'not_executed: candidate and manifest SHA-256 values plus AGENTBRIDGE_PI_JOB_ID are required\n' >&2
  exit 3
fi

binary_digest=$(shasum -a 256 "$agentbridge_bin" | awk '{print $1}')
manifest_digest=$(shasum -a 256 "$AGENTBRIDGE_CANDIDATE_MANIFEST" | awk '{print $1}')
if [[ "${binary_digest,,}" != "${AGENTBRIDGE_CANDIDATE_SHA256,,}" || "${manifest_digest,,}" != "${AGENTBRIDGE_MANIFEST_SHA256,,}" ]]; then
  printf 'candidate_mismatch: immutable binary or manifest digest differs\n' >&2
  exit 1
fi

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
