# AgentBridge 2.0 Cutover Design

**Status:** Approved

**Scope:** Task 3 of the AgentBridge 2.0 implementation plan.

## Goal

Provide an explicit, one-way migration command that converts a recognized
AgentBridge 1.x or donor database into the canonical 2.0 SQLite schema without
mutating the active database until a verified backup exists.

## Design

`agentbridge migrate --database <path>` is the only entry point for the
1.x-to-2.0 transformation. It acquires a migration filesystem lock, rejects
active writers, runs SQLite integrity and lineage checks, and validates applied
migration names, embedded migration checksums, and the live structural
fingerprint. Unknown or altered databases fail closed.

Before transformation, the command creates a mode-0600 backup with SQLite's
online backup API so freelist pages do not cause a false page-count mismatch. It
independently verifies backup integrity, schema fingerprint, page count, and
representative row counts, then writes a manifest containing source and backup
hashes, the source fingerprint, tool version, and timestamp.

The transformation runs in one SQLite transaction. It creates the embedded
execution-kernel schema, maps legacy tasks to `local_tasks` and synthetic
legacy executions, preserves sessions and all recognized execution evidence,
records donor retry/operator intent, and removes legacy tables before commit.
Telegram presentation identifiers are not copied into domain records and no
compatibility views remain. The new migration ledger records the schema
checksum, structural fingerprint, and application time.

Fresh empty databases may be opened only through `OpenV2`, which bootstraps
the v2 schema and ledger. `OpenV2` refuses recognized legacy databases until
the explicit migration command succeeds; the existing 1.x `Open` path remains
unchanged until the later atomic activation task.

## Failure behavior and rollback

Any preflight, backup, mapping, validation, or commit failure aborts before
activation or rolls back the cutover transaction. Failure-injection tests
compare the active database hash with its pre-cutover hash. After a successful
cutover, binary rollback requires restoring the verified pre-cutover backup and
explicitly loses activity written after the cutover.

## Verification

Focused tests cover public, attachment-adopted, donor, unknown, corrupt, and
actively written lineages; deterministic fingerprints; fresh bootstrap; full
record/link preservation; backup manifests; failure injection; rollback
wording; CLI registration; and refusal while the daemon is running. The Task 3
focused suite and the wider Go suite must pass before the task commit.
