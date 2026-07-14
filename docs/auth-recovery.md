# Subscription Authentication Recovery

Authentication recovery is a privileged operation. Perform it from the private
dashboard over the tailnet or an authenticated shell. Never send device codes,
OAuth tokens, login URLs containing secrets, or credential files through
Telegram.

## Detection

An authentication failure pauses affected work, records a redacted incident,
and leaves repository delivery disabled. Confirm the provider named by the
incident and inspect its official CLI status command. Do not repeatedly restart
the bridge; restart loops do not repair expired sessions.

## Codex CLI

1. Pause or cancel active Codex tasks.
2. Stop the AgentBridge service.
3. Use the official Codex device-login flow as the service account.
4. Confirm the CLI reports a valid ChatGPT subscription session.
5. Run `agentbridge doctor`, restart the service, and resume only the intended task.

Codex owns its session files. Do not copy them into AgentBridge configuration,
Telegram, logs, or backups.

## Claude Code

1. Pause or cancel active Claude tasks and stop the service.
2. Use Claude Code's official subscription setup-token flow as the service account.
3. Transfer the resulting subscription credential directly into the
   `claude_oauth_token` systemd credential source with mode `0600`.
4. Confirm official CLI authentication status without a model turn.
5. Run `agentbridge doctor`, restart, and resume the intended task.

## Verification

After either recovery, confirm that the dashboard is still loopback-only, the
Telegram identity allowlist is unchanged, no provider API-key credentials are
present, and a usage/status command succeeds without a model turn. Record the
time and provider version, but never credential contents.
