# Operations

## Runtime layout

The provided user service uses these generic per-user locations:

| Purpose | Path |
| --- | --- |
| Binary | `$HOME/.local/bin/agentbridge` |
| Private configuration | `$HOME/.config/agentbridge/config.yaml` |
| systemd credential sources | `$HOME/.config/agentbridge/credentials/` |
| Database and managed data | `$HOME/.local/share/agentbridge/` |
| Installed maintenance scripts | `$HOME/.local/lib/agentbridge/scripts/` |

Keep every directory private to the service account. The dashboard must listen
on `127.0.0.1:8787`. Use private Tailscale Serve HTTPS to proxy to that address.
Do not enable public ingress. Verify the tailnet access policy from both an
allowed and an unrelated device.

The hardened unit makes the system filesystem and home directory read-only
except for AgentBridge data, cache, and the official CLI state directories. If
a configured checkout is elsewhere, add its exact absolute path with a
`ReadWritePaths=` entry in a systemd drop-in. Never grant the whole home
directory or filesystem. Re-run `systemd-analyze --user security
agentbridge.service` after changing the sandbox.

## Install

```sh
make test build
AGENTBRIDGE_BINARY=./bin/agentbridge ./deploy/install.sh
$HOME/.local/bin/agentbridge doctor --config $HOME/.config/agentbridge/config.yaml
systemctl --user enable --now agentbridge.service agentbridge-backup.timer
```

Populate the Telegram bot systemd credential source without exposing its
contents in shell history. The file must be mode `0600`; its parent directory
must be mode `0700`. Pair one numeric Telegram user in a private chat. Log out
from any provider API-key authentication before starting; only official
subscription CLI sessions owned by each provider are supported.

Before filling the final numeric IDs in the private configuration, run the
one-time pairing flow against the same credential directory:

```sh
CREDENTIALS_DIRECTORY="$HOME/.config/agentbridge/credentials" \
  "$HOME/.local/bin/agentbridge" pair telegram \
  --config "$HOME/.config/agentbridge/config.yaml"
```

The command prints a short-lived `/pair <nonce>` instruction first. Send it to
the bot in a private Telegram chat; the command then prints only
`telegram_user_id` and `telegram_chat_id`. Put those numeric values in
`allowed_user_ids` and `paired_chat_id`, then run `agentbridge doctor` again.

## Managed enrollment and mode

Enrollment is an explicit two-step exchange. The first command creates (or
loads) an owner-only Ed25519 device key and prints a public claim request; it
never prints the private key:

```sh
agentbridge enroll --data-dir "$HOME/.local/share/agentbridge" \
  --claim-id <claim> --organization-id <organization> --device-id <device> \
  --browser-fingerprint <confirmed-fingerprint> --output /path/to/request.json
```

After the controller returns its signed challenge, produce the proof and
persist the public enrollment record:

```sh
agentbridge enroll --data-dir "$HOME/.local/share/agentbridge" \
  --claim-id <claim> --organization-id <organization> --device-id <device> \
  --browser-fingerprint <confirmed-fingerprint> --challenge /path/to/challenge.json
```

Set `mode: managed` and the `managed.gateway_url`, organization, and device
fields in the private configuration before starting the daemon. `mode:
standalone` remains the default. A durable mode switch is rejected while an
execution is active; a missing, mismatched, revoked, or quarantined identity
fails managed startup and requires explicit re-enrollment.

Use `loginctl enable-linger "$USER"` only if the service must survive logout.
Review that choice against the security policy of the host.

## Daily checks

```sh
systemctl --user status agentbridge.service
journalctl --user -u agentbridge.service --since today
$HOME/.local/lib/agentbridge/scripts/pi-smoke.sh
```

The Pi script is a controlled release gate, not a routine laptop check. It
returns exit code `3` with `not_executed` unless `RUN_PI_SMOKE=1`, the host is
ARM64 Linux, and `AGENTBRIDGE_CANDIDATE_MANIFEST` names the immutable candidate
manifest. Never convert that result into a passing release attestation.

Observable logs must be redacted. Treat unexpected command lines, files,
approval requests, repeated restarts, or repository changes as an incident.

## Backup and retention

The daily timer calls SQLite's online backup API, checks the new snapshot, then
applies retention. It does not copy a live WAL database. Defaults are:

- verified database backups retained for 14 days;
- user-visible redacted events on inactive tasks retained for 30 days;
- attachments and failed/canceled worktrees on inactive tasks retained for 7 days.

List one task ID per line in
`$HOME/.local/share/agentbridge/pinned-task-ids` to exempt it from event and
artifact retention. Active task states are always exempt. Override durations
in `$HOME/.config/agentbridge/backup.env` with positive integer day counts.

Run the supported v2 backup and non-destructive restore check commands:

```sh
systemctl --user start agentbridge-backup.service
$HOME/.local/bin/agentbridge doctor \
  --database "$HOME/.local/share/agentbridge/agentbridge.db" --json
$HOME/.local/bin/agentbridge restore-check \
  --backup "$HOME/.local/share/agentbridge/backups" \
  --work-dir "$HOME/.cache/agentbridge/restore-check"
```

Managed backups include the public device fingerprint, organization/device
IDs, highest accepted controller epoch, mode, and managed cursor facts. They
never include the device private key or provider/Git credentials. A restore on
another machine reports `re-enrollment required`; an in-place restore may pass
managed readiness only when the existing OS-protected key and enrollment
record still match the backup facts.

Copy encrypted backups to a separate device or storage account according to
your own threat model. Never include credential files or CLI session homes in
the database backup.

## Uninstall

`./deploy/uninstall.sh` removes binaries and units but intentionally preserves
configuration, credentials, data, worktrees, and backups. Review and remove
those separately only after a verified backup and explicit operator decision.
