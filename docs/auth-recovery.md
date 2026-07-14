# Interactive Subscription Authentication Recovery

Authentication recovery is a privileged, interactive operation. Use the
Tailscale-only dashboard from an allowed device. Never send device codes,
login URLs containing secrets, CLI session files, or other authentication
material through Telegram.

## Dashboard-supervised recovery

When a provider reports expired authentication, AgentBridge pauses affected
tasks and records a redacted incident. The daemon remains running so the
dashboard can supervise recovery while unrelated healthy work remains visible.

1. Open the private dashboard over Tailscale and inspect the provider incident.
2. Start recovery for only the affected provider.
3. AgentBridge launches the official login command in a supervised local
   session and streams only safe, observable status to the dashboard.
4. Complete the provider's interactive authorization on the allowed device.
5. Confirm the official CLI status, close the incident, and explicitly resume
   the intended paused task.

Each CLI owns and persists its subscription session files. AgentBridge does
not copy provider sessions into its configuration, database, systemd
credentials, Telegram messages, logs, or backups.

### Codex CLI

The supervised recovery command is:

```sh
codex login --device-auth
```

Complete the device authorization in the browser, then confirm the CLI reports
a valid ChatGPT subscription session. A successful login does not automatically
resume or deliver any paused repository task.

### Claude Code

The supervised recovery command is:

```sh
claude auth login --claudeai
```

Complete the Claude subscription authorization in the browser, then confirm
the official CLI authentication status. A successful login does not
automatically resume or deliver any paused repository task.

## Authenticated-shell fallback

If the private dashboard is unavailable, connect to the service account over
the tailnet. Pause active work and stop AgentBridge only if necessary to avoid
a conflicting provider session. Run the same official login command directly
as the service account, verify CLI status, restart the daemon if it was stopped,
and run the smoke checks. Do not copy session files or tokens into AgentBridge.

## Verification

Confirm that the dashboard remains loopback-only, the Tailscale identity policy
and Telegram numeric allowlist are unchanged, no provider API-key credentials
are present, and a provider usage/status command succeeds without a model turn.
Record the recovery time and provider version, never authentication material.
