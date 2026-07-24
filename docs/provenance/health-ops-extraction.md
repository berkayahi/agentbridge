# Health and operations provenance

The health contract is intentionally small: liveness proves only that the
process can answer, while readiness checks injected database, identity, spool,
runtime, migration/mode, and managed-connector dependencies. It never invokes
a provider, reads a credential, or returns secret material.

`scripts/ops-smoke.sh` uses a supplied binary and optional config/database
paths, so CI can run it against temporary fake-runtime assets. Raspberry Pi
systemd and real-runtime evidence remains a separately controlled hardware
gate. `scripts/pi-smoke.sh` exits `3` with `not_executed` unless it is run on
controlled ARM64 Linux with `RUN_PI_SMOKE=1` and an immutable candidate
manifest. The manifest must be a readable owner-only regular file (not a
symlink); a local macOS run is not treated as ARM evidence.

The controlled candidate manifest binds `product_version`, `build_tag`,
`source_commit`, `artifact_digest`, `goos`, `goarch`, `job_id`, and `nonce`.
The binary reports the first four identity values through `agentbridge version`
and release builds inject them with the Makefile link flags. The smoke compares
those values and the binary/manifest SHA-256 digests before checking the active
user-systemd `MainPID`. It writes an owner-only
`agentbridge.pi.service-preflight.v1` record containing the job/nonce,
measured OS/kernel/systemd/hardware identity, and the running PID digest.
That record deliberately marks the vertical slice and reconnect fields as
`not_executed`; it is preflight evidence, not the final 15G release attestation.
The controlled run then supplies an owner-only
`agentbridge.pi.vertical-slice.v1` report to `scripts/pi-acceptance.sh`.
That verifier binds the report to the preflight digest, candidate/manifest
digests, job nonce, ordered Create → Start → Observe → Approve/Cancel →
Verify → Commit evidence, and duplicate-free reconnect/commit evidence before
writing the owner-only Phase C `agentbridge.pi.acceptance.v1` record. Missing
or synthetic hardware evidence cannot satisfy this verifier; release signing
and independent publication remain separate release gates.
