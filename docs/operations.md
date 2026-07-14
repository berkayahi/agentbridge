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

Populate systemd credential source files without exposing their contents in
shell history. Each file must be mode `0600`; its parent directory must be mode
`0700`. Pair one numeric Telegram user in a private chat. Log out from any
provider API-key authentication before starting; only official subscription
CLI sessions are supported.

Use `loginctl enable-linger "$USER"` only if the service must survive logout.
Review that choice against the security policy of the host.

## Daily checks

```sh
systemctl --user status agentbridge.service
journalctl --user -u agentbridge.service --since today
$HOME/.local/lib/agentbridge/scripts/pi-smoke.sh
```

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

Run a backup and non-destructive restore check:

```sh
systemctl --user start agentbridge-backup.service
$HOME/.local/lib/agentbridge/scripts/restore-check.sh \
  "$HOME/.local/share/agentbridge/backups/<backup-file>.db"
```

Copy encrypted backups to a separate device or storage account according to
your own threat model. Never include credential files or CLI session homes in
the database backup.

## Uninstall

`./deploy/uninstall.sh` removes binaries and units but intentionally preserves
configuration, credentials, data, worktrees, and backups. Review and remove
those separately only after a verified backup and explicit operator decision.
