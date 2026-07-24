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
service_name=${AGENTBRIDGE_SERVICE_NAME:-agentbridge.service}

if [[ ! "$service_name" =~ ^[A-Za-z0-9_.@-]+\.service$ ]]; then
  printf 'not_executed: AGENTBRIDGE_SERVICE_NAME is not a valid user service name\n' >&2
  exit 3
fi

if [[ ! -x "$agentbridge_bin" ]]; then
  resolved_binary=$(command -v "$agentbridge_bin" || true)
  if [[ -z "$resolved_binary" ]]; then
    printf 'not_executed: candidate AgentBridge binary is unavailable\n' >&2
    exit 3
  fi
  agentbridge_bin=$resolved_binary
fi

if [[ -z "${AGENTBRIDGE_CANDIDATE_MANIFEST:-}" || ! -f "${AGENTBRIDGE_CANDIDATE_MANIFEST:-}" || -L "${AGENTBRIDGE_CANDIDATE_MANIFEST:-}" ]]; then
  printf 'not_executed: AGENTBRIDGE_CANDIDATE_MANIFEST must identify the immutable candidate manifest\n' >&2
  exit 3
fi
if [[ -z "${AGENTBRIDGE_CANDIDATE_SHA256:-}" || -z "${AGENTBRIDGE_MANIFEST_SHA256:-}" || -z "${AGENTBRIDGE_PI_JOB_ID:-}" || -z "${AGENTBRIDGE_PI_NONCE:-}" || -z "${AGENTBRIDGE_PI_ATTESTATION_PATH:-}" ]]; then
	printf 'not_executed: candidate/manifest digests, job ID, nonce, and attestation path are required\n' >&2
	exit 3
fi
if [[ "$AGENTBRIDGE_PI_ATTESTATION_PATH" != /* ]]; then
	printf 'not_executed: AGENTBRIDGE_PI_ATTESTATION_PATH must be absolute\n' >&2
	exit 3
fi
for value in "$AGENTBRIDGE_PI_JOB_ID" "$AGENTBRIDGE_PI_NONCE"; do
	if [[ ! "$value" =~ ^[A-Za-z0-9._:-]+$ ]]; then
		printf 'not_executed: job ID and nonce contain unsupported characters\n' >&2
		exit 3
	fi
done

manifest_mode=$(stat -c '%a' "$AGENTBRIDGE_CANDIDATE_MANIFEST" 2>/dev/null || stat -f '%Lp' "$AGENTBRIDGE_CANDIDATE_MANIFEST")
if [[ ! "$manifest_mode" =~ ^[0-7]{3,4}$ || "${manifest_mode: -2}" != "00" || ! -r "$AGENTBRIDGE_CANDIDATE_MANIFEST" ]]; then
	printf 'not_executed: candidate manifest must be a readable owner-only regular file\n' >&2
	exit 3
fi

for value in "$AGENTBRIDGE_CANDIDATE_SHA256" "$AGENTBRIDGE_MANIFEST_SHA256"; do
	if [[ ! "$value" =~ ^[0-9a-fA-F]{64}$ ]]; then
		printf 'not_executed: candidate and manifest digests must be SHA-256 values\n' >&2
		exit 3
	fi
done

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
    return
  fi
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
    return
  fi
  printf 'not_executed: no SHA-256 utility is available\n' >&2
  exit 3
}

binary_digest=$(sha256_file "$agentbridge_bin")
manifest_digest=$(sha256_file "$AGENTBRIDGE_CANDIDATE_MANIFEST")
if [[ "${binary_digest,,}" != "${AGENTBRIDGE_CANDIDATE_SHA256,,}" || "${manifest_digest,,}" != "${AGENTBRIDGE_MANIFEST_SHA256,,}" ]]; then
  printf 'candidate_mismatch: immutable binary or manifest digest differs\n' >&2
  exit 1
fi
manifest_value() {
	local field=$1 match
	match=$(grep -Eo "\"${field}\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" "$AGENTBRIDGE_CANDIDATE_MANIFEST" | head -n 1 || true)
	if [[ -n "$match" ]]; then
		printf '%s\n' "$match" | sed -E 's/^[^:]+:[[:space:]]*"([^"]*)".*$/\1/'
	fi
}

version_output=$("$agentbridge_bin" version)
printf '%s\n' "$version_output"
if [[ ! "$version_output" =~ ^agentbridge[[:space:]]+([^[:space:]]+)[[:space:]]+\(build[[:space:]]+([^,]+),[[:space:]]+commit[[:space:]]+([^,]+),[[:space:]]+built[[:space:]]+(.+)\)$ ]]; then
	printf 'candidate_mismatch: binary version output has no release identity\n' >&2
	exit 1
fi
product_version=${BASH_REMATCH[1]}
build_tag=${BASH_REMATCH[2]}
source_commit=${BASH_REMATCH[3]}
build_date=${BASH_REMATCH[4]}
manifest_product_version=$(manifest_value product_version)
manifest_build_tag=$(manifest_value build_tag)
manifest_source_commit=$(manifest_value source_commit)
manifest_artifact_digest=$(manifest_value artifact_digest)
manifest_goos=$(manifest_value goos)
manifest_goarch=$(manifest_value goarch)
manifest_job_id=$(manifest_value job_id)
manifest_nonce=$(manifest_value nonce)
if [[ ! "$product_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ || ! "$source_commit" =~ ^[0-9a-fA-F]{40}$ || -z "$build_tag" || "$product_version" != "$manifest_product_version" || "$build_tag" != "$manifest_build_tag" || "$source_commit" != "$manifest_source_commit" || "${manifest_artifact_digest,,}" != "${binary_digest,,}" || "$manifest_goos" != "linux" || "$manifest_goarch" != "arm64" || "$manifest_job_id" != "$AGENTBRIDGE_PI_JOB_ID" || "$manifest_nonce" != "$AGENTBRIDGE_PI_NONCE" ]]; then
	printf 'candidate_mismatch: embedded release identity does not match candidate manifest\n' >&2
	exit 1
fi
"$agentbridge_bin" doctor --config "$config"
if [[ -f "$database" ]]; then
  "$agentbridge_bin" doctor --database "$database" --json >/dev/null
fi
if ! command -v systemctl >/dev/null 2>&1; then
  printf 'not_executed: systemd user manager is required\n' >&2
  exit 3
fi
systemctl --user is-active --quiet "$service_name"
systemctl --user is-active --quiet agentbridge-backup.timer
main_pid=$(systemctl --user show --property=MainPID --value "$service_name")
if [[ ! "$main_pid" =~ ^[1-9][0-9]*$ || ! -e "/proc/$main_pid/exe" ]]; then
  printf 'candidate_mismatch: active AgentBridge service PID is unavailable\n' >&2
  exit 1
fi
service_binary=$(readlink -f "/proc/$main_pid/exe")
candidate_binary=$(readlink -f "$agentbridge_bin")
if [[ -z "$service_binary" || -z "$candidate_binary" || "$service_binary" != "$candidate_binary" ]]; then
  printf 'candidate_mismatch: systemd service is not running the candidate binary\n' >&2
  exit 1
fi
running_digest=$(sha256_file "$service_binary")
if [[ "${running_digest,,}" != "${binary_digest,,}" ]]; then
	printf 'candidate_mismatch: active systemd binary digest differs\n' >&2
	exit 1
fi

hardware_model=unknown
if [[ -r /proc/device-tree/model ]]; then
	hardware_model=$(tr -d '\0' < /proc/device-tree/model | tr '\n' ' ')
elif [[ -r /sys/devices/virtual/dmi/id/product_name ]]; then
	hardware_model=$(tr -d '\0' < /sys/devices/virtual/dmi/id/product_name | tr '\n' ' ')
fi
os_name=unknown
if [[ -r /etc/os-release ]]; then
	os_name=$(sed -n -E 's/^PRETTY_NAME="?([^"].*[^" ]|[^" ]?)"?$/\1/p' /etc/os-release | head -n 1 || true)
fi
systemd_version=$(systemctl --version | awk 'NR == 1 { print $2 }')
json_escape() {
	local value=$1
	value=${value//\\/\\\\}
	value=${value//\"/\\\"}
	value=${value//$'\n'/ }
	value=${value//$'\r'/ }
	value=${value//$'\t'/ }
	printf '%s' "$value"
}

attestation_path=$AGENTBRIDGE_PI_ATTESTATION_PATH
attestation_dir=$(dirname -- "$attestation_path")
if [[ -L "$attestation_dir" || -e "$attestation_dir" && ! -d "$attestation_dir" ]]; then
	printf 'candidate_mismatch: attestation parent is not a directory\n' >&2
	exit 1
fi
mkdir -p -- "$attestation_dir"
chmod 0700 -- "$attestation_dir"
if [[ "$(stat -c '%a' "$attestation_dir")" != "700" ]]; then
	printf 'candidate_mismatch: attestation parent is not owner-only\n' >&2
	exit 1
fi
if [[ -e "$attestation_path" || -L "$attestation_path" ]]; then
	printf 'candidate_mismatch: attestation path already exists\n' >&2
	exit 1
fi
attestation_tmp=$(mktemp "$attestation_path.tmp.XXXXXX")
trap 'rm -f -- "$attestation_tmp"' EXIT
{
	printf '{\n'
	printf '  "schema": "agentbridge.pi.service-preflight.v1",\n'
	printf '  "status": "preflight_pass",\n'
	printf '  "job_id": "%s",\n' "$(json_escape "$AGENTBRIDGE_PI_JOB_ID")"
	printf '  "nonce": "%s",\n' "$(json_escape "$AGENTBRIDGE_PI_NONCE")"
	printf '  "binary_sha256": "%s",\n' "$binary_digest"
	printf '  "manifest_sha256": "%s",\n' "$manifest_digest"
	printf '  "product_version": "%s",\n' "$(json_escape "$product_version")"
	printf '  "build_tag": "%s",\n' "$(json_escape "$build_tag")"
	printf '  "source_commit": "%s",\n' "$(json_escape "$source_commit")"
	printf '  "build_date": "%s",\n' "$(json_escape "$build_date")"
	printf '  "platform": {"goos": "linux", "goarch": "arm64"},\n'
	printf '  "hardware": {"model": "%s", "kernel": "%s", "os": "%s", "systemd": "%s"},\n' "$(json_escape "$hardware_model")" "$(json_escape "$(uname -r)")" "$(json_escape "$os_name")" "$(json_escape "$systemd_version")"
	printf '  "service": {"name": "%s", "main_pid": %s, "binary_sha256": "%s"},\n' "$(json_escape "$service_name")" "$main_pid" "$running_digest"
	printf '  "vertical_slice": "not_executed",\n'
	printf '  "reconnect": "not_executed"\n'
	printf '}\n'
} > "$attestation_tmp"
chmod 0600 "$attestation_tmp"
if ! ln -- "$attestation_tmp" "$attestation_path"; then
	printf 'candidate_mismatch: could not create immutable attestation\n' >&2
	exit 1
fi
rm -f -- "$attestation_tmp"
trap - EXIT

printf 'AgentBridge Raspberry Pi service preflight passed; vertical-slice/reconnect evidence remains required.\n'
