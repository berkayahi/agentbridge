# Architecture

AgentBridge is a local operations daemon for subscription-authenticated agent
CLIs. Its durable v2 kernel owns task, execution, session, repository, policy,
and event identity; configuration owns only local bindings and presentation
choices. It is deliberately generic: configuration owns identities,
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

SQLite v2 stores local tasks, executions, redacted observable events, sessions,
approvals, intents, and fencing evidence. A repository lease provides one
writer per profile. Provider adapters translate observable CLI protocol events
into the shared work model; hidden
reasoning is neither collected nor claimed.

Standalone and managed controllers share the kernel command boundary. Managed
frames are accepted only after enrollment-pinned signature, epoch, replay, and
durable inbox checks; Telegram and the local web surface remain projections.

The HTTP server binds to loopback. Private HTTPS ingress terminates in the
tailnet and forwards to that loopback listener. Sensitive authentication
recovery is never performed in Telegram.

See [operations.md](operations.md) for the runtime layout and
[incident-response.md](incident-response.md) for containment.
