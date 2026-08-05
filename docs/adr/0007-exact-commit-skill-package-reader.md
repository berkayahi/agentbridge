# ADR 0007: Exact-commit skill package reader

Status: accepted, 2026-08-05.

AgentBridge exposes a local `repository-skills-v1` contract for clients that
need committed skill-package sources without learning or supplying a checkout
path. A request names one configured repository profile, one full expected
commit SHA, and an optional normalized repository scope. The reader verifies
the commit identity and returns only regular Git blobs whose basename is
`SKILL.md`.

The contract is not a general repository file API. It rejects symlinks and
non-blob tree entries, applies the normal evidence redactor, and enforces fixed
per-file, file-count, and packet-size bounds. Every file has a content digest;
the complete deterministic packet has a result digest. Repository content is
read from Git objects and is never checked out or executed.

AgentBridge deliberately owns only this repository and execution boundary.
Skill adaptation, lifecycle, evaluation, role composition, and selection
remain client product semantics.
