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

Deterministic fixture/dev acceptance can explicitly select
`analysis_fixture.implementation: in_process_deterministic_v1`. This provider
is compiled into AgentBridge and receives only typed commit, role, prompt, and
evidence-reference metadata. It cannot start a process, access a network
client, or open the filesystem. Fixture mode rejects executable/model settings
and starts no external provider process. Outside fixture mode, configured
Codex remains a normal task provider and never receives repository-analysis
eligibility.

Runtime inspection is unavailable. AgentBridge will continue to fail closed
until an executor exists that actually enforces target, filesystem, network,
credential, production-data, and destructive-operation restrictions.
M2 therefore exposes no runtime-inspection config, API, or executor capability;
unknown configuration fields are rejected. The compiled fixture has no process,
filesystem, network, database, credential, host-secret, or mutation dependency,
so it cannot inspect a runtime or reach production state.
