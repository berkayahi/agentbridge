# ADR 0002: Secret-scanning publication gate

Status: Accepted

## Decision

Every AgentBridge commit, checkpoint, push, pull-request publication, review,
and merge request passes the bounded `internal/secretscan` scanner before the
side effect. Findings, oversized inputs, unreadable files, and timeout or
scanner errors block the operation; there is no warn-only or blind-retry path.
The detector uses the public Gitleaks-compatible credential rule profile. The
release image pins Gitleaks CLI `v8.24.2` for independent release verification;
the broker's local detector remains available for offline device operation and
uses the same fail-closed rule classes without importing provider credentials.

## Threat model and controls

The publication boundary may contain staged source, generated files, binaries,
or encoded credentials. Scans are path-explicit, bounded to 32 MiB per file and
64 MiB per operation, time-limited, and report only rule IDs and paths. Local
allowlists are exact path/rule/token matches and are never uploaded. Credentials
are redacted from receipts and subprocess output.

## Alternatives and operations

Warn-only scanning, ad-hoc regular expressions in the delivery controller, and
provider-side scanning were rejected because an error or provider outage would
permit an unsafe publication. Gitleaks `v8.24.2` is the pinned maintained
release verifier owned by the AgentBridge release operator; upgrades require a
new ADR review and fixture run. A scanner failure blocks publication and keeps
the staged worktree intact. Rollback disables publication or restores the prior
accepted scanner binary; it never silently weakens the gate.

## Test vectors

The release fixture set covers private keys, GitHub/OpenAI/Telegram/AWS tokens,
credential assignments, base64-encoded values, binaries, oversized files,
allowlisted false positives, timeout, and unreadable inputs.
