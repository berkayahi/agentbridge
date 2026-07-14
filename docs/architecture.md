# Architecture

AgentBridge is a single-user operations daemon for subscription-authenticated
agent CLIs. It is deliberately generic: configuration owns identities,
repository paths, verification commands, delivery policy, and deployment URLs.

## Trust boundaries

```text
Telegram private chat ──> command/auth boundary ──> durable task scheduler
                                                      │
localhost dashboard ──> tailnet identity boundary ────┤
                                                      ├─> Codex CLI
                                                      ├─> Claude Code
                                                      └─> isolated Git worktree
                                                              │
                                                              └─> exact configured ref
```

Telegram and dashboard input never selects a filesystem path, remote, or push
ref. The repository profile supplies those values. Delivery is disabled by
default, requires passing verification commands, and only targets one exact
configured non-production ref.

SQLite stores tasks, redacted observable events, sessions, approvals, and
leases. A repository lease provides one writer per profile. Provider adapters
translate observable CLI protocol events into the shared task model; hidden
reasoning is neither collected nor claimed.

The HTTP server binds to loopback. Private HTTPS ingress terminates in the
tailnet and forwards to that loopback listener. Sensitive authentication
recovery is never performed in Telegram.

See [operations.md](operations.md) for the runtime layout and
[incident-response.md](incident-response.md) for containment.
