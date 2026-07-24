#!/usr/bin/env bash
set -euo pipefail

if [[ ${RUN_PI_ACCEPTANCE:-0} != "1" ]]; then
  printf 'not_executed: controlled Raspberry Pi acceptance requires RUN_PI_ACCEPTANCE=1\n' >&2
  exit 3
fi

for name in AGENTBRIDGE_PI_PREFLIGHT_ATTESTATION AGENTBRIDGE_PI_VERTICAL_EVIDENCE AGENTBRIDGE_PI_ACCEPTANCE_ATTESTATION_PATH AGENTBRIDGE_CANDIDATE_SHA256 AGENTBRIDGE_MANIFEST_SHA256 AGENTBRIDGE_PI_JOB_ID AGENTBRIDGE_PI_NONCE; do
  if [[ -z ${!name:-} ]]; then
    printf 'not_executed: %s is required\n' "$name" >&2
    exit 3
  fi
done

service_name=${AGENTBRIDGE_SERVICE_NAME:-agentbridge.service}
if [[ ! "$service_name" =~ ^[A-Za-z0-9_.@-]+\.service$ ]]; then
  printf 'not_executed: AGENTBRIDGE_SERVICE_NAME is not a valid user service name\n' >&2
  exit 3
fi
export AGENTBRIDGE_SERVICE_NAME="$service_name"

for path in "$AGENTBRIDGE_PI_PREFLIGHT_ATTESTATION" "$AGENTBRIDGE_PI_VERTICAL_EVIDENCE" "$AGENTBRIDGE_PI_ACCEPTANCE_ATTESTATION_PATH"; do
  if [[ "$path" != /* ]]; then
    printf 'not_executed: acceptance paths must be absolute\n' >&2
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

for value in "$AGENTBRIDGE_CANDIDATE_SHA256" "$AGENTBRIDGE_MANIFEST_SHA256"; do
  if [[ ! "$value" =~ ^[0-9a-fA-F]{64}$ ]]; then
    printf 'not_executed: candidate and manifest digests must be SHA-256 values\n' >&2
    exit 3
  fi
done
for value in "$AGENTBRIDGE_PI_JOB_ID" "$AGENTBRIDGE_PI_NONCE"; do
  if [[ ! "$value" =~ ^[A-Za-z0-9._:-]+$ ]]; then
    printf 'not_executed: job ID and nonce contain unsupported characters\n' >&2
    exit 3
  fi
done

for path in "$AGENTBRIDGE_PI_PREFLIGHT_ATTESTATION" "$AGENTBRIDGE_PI_VERTICAL_EVIDENCE"; do
  if [[ ! -f "$path" || -L "$path" ]]; then
    printf 'not_executed: acceptance evidence is not a regular file: %s\n' "$path" >&2
    exit 3
  fi
done
if ! command -v python3 >/dev/null 2>&1; then
  printf 'not_executed: Python 3 is required by the acceptance verifier\n' >&2
  exit 3
fi

preflight_sha256=$(sha256_file "$AGENTBRIDGE_PI_PREFLIGHT_ATTESTATION")
evidence_sha256=$(sha256_file "$AGENTBRIDGE_PI_VERTICAL_EVIDENCE")
candidate_sha256=$(printf '%s' "$AGENTBRIDGE_CANDIDATE_SHA256" | tr '[:upper:]' '[:lower:]')
manifest_sha256=$(printf '%s' "$AGENTBRIDGE_MANIFEST_SHA256" | tr '[:upper:]' '[:lower:]')

python3 - "$AGENTBRIDGE_PI_PREFLIGHT_ATTESTATION" "$AGENTBRIDGE_PI_VERTICAL_EVIDENCE" "$AGENTBRIDGE_PI_ACCEPTANCE_ATTESTATION_PATH" "$AGENTBRIDGE_PI_JOB_ID" "$AGENTBRIDGE_PI_NONCE" "$candidate_sha256" "$manifest_sha256" "$preflight_sha256" "$evidence_sha256" <<'PY'
import json
import os
import pathlib
import stat
import sys
import tempfile


class AcceptanceError(Exception):
    pass


def no_duplicate_pairs(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise AcceptanceError(f"duplicate JSON key: {key}")
        value[key] = item
    return value


def read_object(path_text):
    path = pathlib.Path(path_text)
    try:
        info = path.lstat()
    except OSError as error:
        raise AcceptanceError(f"read evidence {path}: {error}") from error
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077:
        raise AcceptanceError(f"evidence is not an owner-only regular file: {path}")
    data = path.read_bytes()
    if len(data) > 1 << 20:
        raise AcceptanceError(f"evidence is too large: {path}")
    try:
        value = json.loads(data, object_pairs_hook=no_duplicate_pairs)
    except (ValueError, TypeError) as error:
        raise AcceptanceError(f"invalid evidence JSON {path}: {error}") from error
    if not isinstance(value, dict):
        raise AcceptanceError(f"evidence root is not an object: {path}")
    return value


def required(mapping, key, kind):
    value = mapping.get(key)
    if not isinstance(value, kind):
        raise AcceptanceError(f"{key} has the wrong type")
    return value


def nonempty(mapping, key):
    value = required(mapping, key, str).strip()
    if not value:
        raise AcceptanceError(f"{key} is empty")
    return value


def exact(mapping, key, expected):
    if mapping.get(key) != expected:
        raise AcceptanceError(f"{key} does not match the acceptance inputs")


def validate_sha(value, name):
    if not isinstance(value, str) or len(value) != 64:
        raise AcceptanceError(f"{name} is not a SHA-256 value")
    try:
        int(value, 16)
    except ValueError as error:
        raise AcceptanceError(f"{name} is not a SHA-256 value") from error
    return value.lower()


def validate_ids(values, name):
    if not isinstance(values, list) or not values or any(not isinstance(value, str) or not value.strip() for value in values):
        raise AcceptanceError(f"{name} must contain nonempty IDs")
    if len(values) != len(set(values)):
        raise AcceptanceError(f"{name} contains duplicate IDs")
    return values


def positive_int(value, name):
    if not isinstance(value, int) or isinstance(value, bool) or value <= 0:
        raise AcceptanceError(f"{name} is invalid")
    return value


def nonnegative_int(value, name):
    if not isinstance(value, int) or isinstance(value, bool) or value < 0:
        raise AcceptanceError(f"{name} is invalid")
    return value


preflight_path, evidence_path, output_path = sys.argv[1:4]
job_id, nonce = sys.argv[4:6]
candidate_sha256, manifest_sha256 = sys.argv[6:8]
preflight_sha256, evidence_sha256 = sys.argv[8:10]
preflight = read_object(preflight_path)
evidence = read_object(evidence_path)

exact(preflight, "schema", "agentbridge.pi.service-preflight.v1")
exact(preflight, "status", "preflight_pass")
exact(preflight, "job_id", job_id)
exact(preflight, "nonce", nonce)
exact(preflight, "binary_sha256", candidate_sha256)
exact(preflight, "manifest_sha256", manifest_sha256)
exact(preflight, "platform", {"goos": "linux", "goarch": "arm64"})
exact(preflight, "vertical_slice", "not_executed")
exact(preflight, "reconnect", "not_executed")
validate_sha(nonempty(preflight, "binary_sha256"), "preflight binary_sha256")
validate_sha(nonempty(preflight, "manifest_sha256"), "preflight manifest_sha256")
nonempty(preflight, "product_version")
nonempty(preflight, "build_tag")
source_commit = nonempty(preflight, "source_commit")
if len(source_commit) != 40:
    raise AcceptanceError("preflight source_commit is not a full commit")
int(source_commit, 16)
hardware = required(preflight, "hardware", dict)
service = required(preflight, "service", dict)
nonempty(hardware, "model")
nonempty(hardware, "kernel")
nonempty(hardware, "os")
nonempty(hardware, "systemd")
expected_service = os.environ.get("AGENTBRIDGE_SERVICE_NAME", "agentbridge.service")
exact(service, "name", expected_service)
positive_int(service.get("main_pid"), "preflight service main_pid")
exact(service, "binary_sha256", candidate_sha256)

exact(evidence, "schema", "agentbridge.pi.vertical-slice.v1")
exact(evidence, "status", "pass")
exact(evidence, "job_id", job_id)
exact(evidence, "nonce", nonce)
exact(evidence, "binary_sha256", candidate_sha256)
exact(evidence, "candidate_sha256", candidate_sha256)
exact(evidence, "manifest_sha256", manifest_sha256)
exact(evidence, "preflight_sha256", preflight_sha256)
exact(evidence, "platform", {"goos": "linux", "goarch": "arm64"})

vertical = required(evidence, "vertical_slice", dict)
exact(vertical, "status", "pass")
task_id = nonempty(vertical, "task_id")
lineage = required(vertical, "lineage", dict)
for key in ("project_id", "board_id", "repository_id", "execution_id", "session_id"):
    nonempty(lineage, key)
steps = required(vertical, "steps", list)
step_names = [required(step, "name", str) for step in steps if isinstance(step, dict)]
if len(step_names) != len(steps) or step_names != ["create", "start", "observe", "approve_or_cancel", "verify", "commit"]:
    raise AcceptanceError("vertical slice steps are incomplete or out of order")
for step in steps:
    exact(step, "status", "pass")

events = required(vertical, "events", list)
if not events:
    raise AcceptanceError("vertical slice has no ordered events")
event_ids = []
last_cursor = 0
for event in events:
    if not isinstance(event, dict):
        raise AcceptanceError("vertical slice event is not an object")
    cursor = event.get("cursor")
    if not isinstance(cursor, int) or isinstance(cursor, bool) or cursor <= last_cursor:
        raise AcceptanceError("vertical slice event cursors are not strictly increasing")
    last_cursor = cursor
    event_ids.append(nonempty(event, "id"))
    exact(event, "task_id", task_id)
validate_ids(event_ids, "vertical slice event IDs")

commit = required(vertical, "commit", dict)
commit_receipt_id = nonempty(commit, "receipt_id")
commit_sha = nonempty(commit, "commit_sha")
if len(commit_sha) != 40:
    raise AcceptanceError("vertical slice commit SHA is not full length")
int(commit_sha, 16)
nonempty(commit, "remote_ref")

reconnect = required(evidence, "reconnect", dict)
exact(reconnect, "status", "pass")
resume_after = reconnect.get("resume_after_cursor")
nonnegative_int(resume_after, "reconnect resume_after_cursor")
exact(reconnect, "duplicate_event_ids", [])
if reconnect.get("duplicate_commit_receipts") != 0:
    raise AcceptanceError("reconnect produced duplicate commit receipts")
replayed_ids = validate_ids(required(reconnect, "observed_event_ids", list), "reconnect event IDs")
if not set(replayed_ids).issubset(set(event_ids)):
    raise AcceptanceError("reconnect observed an event outside the vertical-slice lineage")
exact(reconnect, "commit_receipt_id", commit_receipt_id)
exact(reconnect, "commit_sha", commit_sha)

output = pathlib.Path(output_path)
parent = output.parent
parent.mkdir(mode=0o700, parents=True, exist_ok=True)
parent_info = parent.lstat()
if stat.S_ISLNK(parent_info.st_mode) or not stat.S_ISDIR(parent_info.st_mode):
    raise AcceptanceError("acceptance attestation parent is not a directory")
os.chmod(parent, 0o700)
try:
    output.lstat()
    raise AcceptanceError("acceptance attestation already exists")
except FileNotFoundError:
    pass

attestation = {
    "schema": "agentbridge.pi.acceptance.v1",
    "status": "PASS",
    "job_id": job_id,
    "nonce": nonce,
    "binary_sha256": candidate_sha256,
    "manifest_sha256": manifest_sha256,
    "preflight_sha256": preflight_sha256,
    "evidence_sha256": evidence_sha256,
    "product_version": preflight["product_version"],
    "build_tag": preflight["build_tag"],
    "source_commit": preflight["source_commit"],
    "platform": preflight["platform"],
    "hardware": hardware,
    "service": service,
    "vertical_slice": {"status": "PASS", "task_id": task_id, "commit_receipt_id": commit_receipt_id, "commit_sha": commit_sha},
    "reconnect": {"status": "PASS", "resume_after_cursor": resume_after, "observed_event_count": len(replayed_ids), "duplicate_commit_receipts": 0},
}
encoded = (json.dumps(attestation, sort_keys=True, separators=(",", ":")) + "\n").encode()
temporary_fd, temporary_name = tempfile.mkstemp(prefix=".agentbridge-pi-acceptance.", dir=str(parent))
try:
    os.fchmod(temporary_fd, 0o600)
    with os.fdopen(temporary_fd, "wb") as temporary:
        temporary.write(encoded)
        temporary.flush()
        os.fsync(temporary.fileno())
    os.link(temporary_name, output, follow_symlinks=False)
finally:
    try:
        os.unlink(temporary_name)
    except FileNotFoundError:
        pass

print("AgentBridge Raspberry Pi acceptance PASS: ordered vertical slice and reconnect evidence verified.")
PY
