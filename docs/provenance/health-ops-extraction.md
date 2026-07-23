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
manifest; a local macOS run is not treated as ARM evidence.
