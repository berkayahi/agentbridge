# Execution isolation

Each execution is bound to one local task, runtime session, repository binding,
policy snapshot, and fencing epoch. Provider processes receive only the scoped
environment and capability needed for that execution. Worktrees are prepared
under the configured private root and are never selected by transport input.

Restart reconciliation pauses work across commit, push, approval, and provider
authentication boundaries unless durable evidence proves a safe continuation.
Ambiguous external effects remain reconciliation-required until a provider or
Git receipt identifies the outcome.

## Repository-understanding analysis

Provider repository analysis is unavailable unless the provider exposes an
explicit isolation attestation covering workspace-only filesystem reads, host
environment exclusion, denied network access, denied production data access,
and denied destructive actions. A provider protocol's workspace-write setting
does not establish workspace-only reads, so the normal Codex app-server
composition does not receive this attestation.

Deterministic fixture/dev acceptance can opt into a narrow pinned executable
seam. The owner configuration must set `analysis_fixture.environment` to
`fixture` or `dev` and pin `analysis_fixture.executable_sha256`. Before launch,
AgentBridge requires that `providers.codex.executable` be a regular non-symlink
file owned by the daemon user, not group/world writable, bounded in size, and
an exact digest match. The pinned process receives only `PATH`; host credentials,
home, daemon, and repository environment variables are excluded. A mismatch
fails daemon composition before the fixture is started.

Optional runtime inspection uses the same fail-closed policy vocabulary and is
limited to fixture/dev targets without host environment, credentials, network,
production data, or destructive actions.
