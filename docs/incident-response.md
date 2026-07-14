# Incident Response

## Priorities

1. Protect credentials and private repository content.
2. Stop unauthorized task execution and delivery.
3. Preserve redacted evidence and a consistent database snapshot.
4. Recover through a reviewed, testable path.

## Containment

Stop `agentbridge.service`. If delivery may be compromised, revoke the
repository-scoped deploy credential and remove its allowed write access. Revoke
the Telegram bot credential if messages or identity checks appear compromised.
Revoke the affected provider CLI session through its official account controls.
Keep the dashboard listener on loopback and remove its private proxy while
investigating remote-access concerns.

Do not delete worktrees, database files, backups, or journals during initial
containment. Do not paste raw logs into public issues.

## Triage

Record UTC times, bridge and CLI versions, systemd state, tailnet identity,
task IDs, configured repository profile, commit IDs, and allowed delivery ref.
Export only redacted observable events. Compare local commits and remote refs
without rewriting history. Determine whether any model process or Git child
survived service shutdown.

## Recovery

Rotate a compromised Telegram bot credential directly into its systemd
credential source. Recover provider authentication through each official CLI,
verify subscription CLI status, restore a checked database only when needed,
and run `agentbridge doctor` plus the smoke script. Re-enable repository write
access last. Resume tasks one at a time and require normal verification before
delivery.

## Follow-up

Document the root cause, affected scope, operator actions, and prevention work
without secrets or private repository data. Add a regression test before the
fix. Review identity allowlists, exact refs, retention pins, backup health, and
provider version freezes.
