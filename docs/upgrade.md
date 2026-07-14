# Upgrade and Rollback

Treat AgentBridge, Codex CLI, and Claude Code as separate change domains.
Never auto-update Codex CLI or Claude Code, especially during unattended
operation or travel. Pin the versions validated by the acceptance suite.

## AgentBridge upgrade

1. Read release notes and back up the database.
2. Build the candidate from a reviewed commit with the pinned Go toolchain.
3. Run `go test -race ./...`, `go vet ./...`, shell syntax checks, and
   `agentbridge doctor` against the private configuration.
4. Save the current binary as `agentbridge.previous` on the same filesystem.
5. Stop `agentbridge.service` and verify no provider or Git child remains.
6. Install the candidate to a temporary name, set mode `0755`, then atomically
   rename it over `$HOME/.local/bin/agentbridge`.
7. Start the service and run the smoke script. Check health, task reconciliation,
   SQLite integrity, private dashboard access, and repository status.
8. Keep the previous binary until the next verified backup and normal task complete.

Do not run database migrations against the only copy of a database. The online
backup and restore check must pass before service start.

## Rollback

Stop the service, atomically restore `agentbridge.previous`, and restart. If an
upgrade changed the database incompatibly, stop again and restore the verified
pre-upgrade database before starting the old binary. Preserve the failed binary,
redacted journal window, and version details for diagnosis.

Upgrade either provider CLI only in a separate maintenance window. Re-run its
protocol compatibility tests and the full acceptance suite before adopting the
new version.
