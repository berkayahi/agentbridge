# Upgrade and Rollback

Treat AgentBridge, Codex CLI, and Claude Code as separate change domains.
Never auto-update Codex CLI or Claude Code, especially during unattended
operation or travel. Pin the versions validated by the acceptance suite.

## AgentBridge upgrade

1. Read release notes and stop the AgentBridge service.
2. Build the candidate from a reviewed commit with the pinned Go toolchain.
3. Run `agentbridge migrate --database <path>` once for a 2.0 cutover. The
   command refuses a running daemon, validates the recognized legacy lineage,
   writes a mode-0600 pre-cutover backup and manifest beside the database, and
   performs the transformation transactionally. Do not delete that backup
   until the upgrade has completed its restore check.
4. Run `go test -race ./...`, `go vet ./...`, shell syntax checks, and
   `agentbridge doctor` against the private configuration. `OpenV2` is the only
   supported fresh/v2 database boundary; ordinary daemon startup does not
   silently migrate a legacy database.
5. Save the current binary as `agentbridge.previous` on the same filesystem.
6. Verify no provider or Git child remains.
7. Install the candidate to a temporary name, set mode `0755`, then atomically
   rename it over `$HOME/.local/bin/agentbridge`.
8. Start the service and run the smoke script. Check health, task reconciliation,
   SQLite integrity, private dashboard access, and repository status.
9. Keep the previous binary until the next verified backup and normal task complete.

Do not run database migrations against the only copy of a database. The
verified pre-cutover backup and restore check must pass before service start.

## Rollback

Stop the service, atomically restore `agentbridge.previous`, and restart. If an
upgrade changed the database incompatibly, stop again and restore the verified
pre-cutover database before starting the old binary. This binary rollback
explicitly loses activity written after the successful cutover. Preserve the
failed binary, backup manifest, redacted journal window, and version details
for diagnosis.

Upgrade either provider CLI only in a separate maintenance window. Re-run its
protocol compatibility tests and the full acceptance suite before adopting the
new version.
