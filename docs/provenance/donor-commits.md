# Donor commit provenance

The unpublished holiday branches are evidence-bearing donor lines, not merge
inputs. Their commit objects are preserved under `refs/archive/agentbridge/*`
and in `/private/tmp/agentbridge-donor-commits.bundle`. Capabilities are
rewritten against the AgentBridge 2.0 kernel with focused tests.

| Historical source ref | Commit | Tree | Subject | Selected semantics | Explicit exclusions |
|---|---|---|---|---|---|
| `codex/holiday-acceptance` | `6fd983f8612ca8b5c45dfab10e4fafbff5761112` | `27f1e6801ffa7915600c568809c92c77db64b212` | `fix: persist Telegram input intent before delivery` | Durable intent claiming, idempotent receipts, restart replay, stale-action tests | Telegram callback payloads and donor SQLite migrations as protocol contracts |
| `codex/holiday-ops` | `d5f86c542c0b248e80f439f093f5d9829ff240ee` | `15ac73690e7869ad5a3a6be57da4f208a7ccda5b` | `fix: finalize retry ownership safely` | Provider failure classification and same-execution retry fencing | Overlapping application composition and migration assumptions |
| `codex/holiday-delivery` | `3ace69e6c8a4cbe4d2fd07e683177fd7e03a3012` | `f75462506b5fd5fc1f5ab156fcfb176c8b3460fc` | `fix: make delivery guard fail closed` | Fail-closed delivery and exact-expected-ref checks | Blanket production/default-branch prohibition |
| `codex/holiday-health` | `1e392d114936c6963f9cdc33f4ceb5e5679d46b1` | `0800f4d03088370c5abd08099ed44edf7dd99986` | `fix: close Pi health readiness gaps` | Bounded health probes and secret-free readiness | Donor deployment topology and unrelated composition |
| `codex/holiday-ops-assets` | `a15b76f3f761d52e8942b8d5bd801b5d3f70a0bf` | `3ca57a5f21c9393253a87299f94dcbc91152d40d` | `feat: add self-healing Pi operations` | Self-healing, health, backup, and restore-check assets after the health model stabilizes | Reuse as signed update or rollback implementation |
