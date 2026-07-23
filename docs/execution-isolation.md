# Execution isolation

Each execution is bound to one local task, runtime session, repository binding,
policy snapshot, and fencing epoch. Provider processes receive only the scoped
environment and capability needed for that execution. Worktrees are prepared
under the configured private root and are never selected by transport input.

Restart reconciliation pauses work across commit, push, approval, and provider
authentication boundaries unless durable evidence proves a safe continuation.
Ambiguous external effects remain reconciliation-required until a provider or
Git receipt identifies the outcome.
