# Phase C local MVP evidence

Baseline: `c335f0fa1bede37fef5ea36ecd92e74370cff3f6`
Branch: `codex/agentbridge-v2`
Checked: 2026-07-24

The local authority, authenticated Unix API, supervised restart harness, typed
paired-device link, durable controller command queue/replay boundary,
dependency-free Desktop host, and Obsidian projection/client are implemented
in the working tree. These focused checks passed:

The Desktop surface is offline-complete at the asset boundary: its stylesheet
uses only local system-font fallbacks and carries no font CDN or other external
resource import. `scripts/local-mvp-smoke.sh` fails closed if that invariant
regresses.

```sh
GOCACHE=/tmp/agentbridge-gocache bash scripts/test-required.sh \
  ./internal/localcontrol ./internal/controller/... ./internal/store/sqlite \
  ./internal/deviceidentity ./internal/managed ./internal/obsidian \
  -run 'Test(Local|PendingApprovals|RemoteObservation|RemoteCommandQueues|DeviceCommandQueue|PairedDeviceSelectionRotationAndFence|Pi|LinkedRuntime|Projection|PersistentClient|RuntimeStore|SyncTaskFile|LocalProcessRestartRecovery)' \
  -count=1
GOCACHE=/tmp/agentbridge-gocache bash scripts/local-mvp-smoke.sh
node --test desktop/host.test.js
node --check desktop/app.js
node --check desktop/host.js
node --check integrations/obsidian/main.js
node --test integrations/obsidian/main.test.js
GOCACHE=/tmp/agentbridge-gocache GOOS=linux GOARCH=arm64 \
  go build -o /tmp/agentbridge-arm64 ./cmd/agentbridge
GOCACHE=/tmp/agentbridge-gocache go test ./... -run '^$' -count=1
GOCACHE=/tmp/agentbridge-gocache go test ./... -count=1
GOCACHE=/tmp/agentbridge-gocache go test -race ./... -count=1
GOCACHE=/tmp/agentbridge-gocache make lint
AGENTBRIDGE_BINARY=./bin/agentbridge bash scripts/ops-smoke.sh
GOCACHE=/tmp/agentbridge-gocache go test -race ./internal/localcontrol ./internal/store/sqlite -count=1
GOCACHE=/tmp/agentbridge-gocache go test ./internal/store/sqlite ./internal/obsidian ./internal/localcontrol -count=1
GOCACHE=/tmp/agentbridge-gocache go test ./cmd/agentbridge -run 'TestDeviceExecutionHandlerObservesShadowEventsAndApprovals' -count=1
```

The standalone composition also supervises the optional device-agent TLS/WSS
listener: an unexpected listener exit is returned from the daemon lifecycle so
the existing systemd restart policy can act.

The device-agent composition is headless by construction. When
`device_agent.enabled` is set, configuration validation does not require the
controller's dashboard/Tailscale/Telegram fields, `serve` does not read the
Telegram credential, and composition omits the standalone controller worker
and owner local API socket. `deploy/systemd/agentbridge-device-agent.service`
is a separate unit with no `LoadCredential=telegram_bot_token`; the controller
unit remains the owner-facing standalone service. The controlled Pi smoke and
acceptance verifier take `AGENTBRIDGE_SERVICE_NAME` so the same immutable
binary/digest gate can attest the dedicated headless unit without weakening
the default controller-service check.

The local and device transports detach their listener state under one lifecycle
lock before closing. A cancellation-versus-supervisor-shutdown race test runs
under the Go race detector so a parent context cannot close transport state at
the same time as the SQLite-owning daemon teardown.
The composed daemon now explicitly closes the authenticated local API and WSS
listeners before the standalone application closes its shared SQLite store;
the shutdown regression checks that the local socket is already gone when the
store close begins.

The controlled Pi smoke resolves the platform SHA-256 utility, requires the
user-systemd manager, compares the active service `MainPID` executable and
digest with the candidate, and verifies the link-injected binary identity
against the candidate manifest. It binds the owner-only preflight record to a
job ID and nonce and records measured hardware/OS/systemd identity. That
preflight record explicitly leaves vertical-slice and reconnect evidence
unexecuted; it is not the final 15G acceptance attestation.

The controlled-hardware acceptance verifier is `scripts/pi-acceptance.sh`. It
consumes an owner-only `agentbridge.pi.vertical-slice.v1` report plus the
preflight record and refuses to emit `agentbridge.pi.acceptance.v1` unless the
same binary/manifest/job nonce is bound, Create → Start → Observe →
Approve/Cancel → Verify → Commit steps are ordered and passed, the commit
receipt is unique, and reconnect replay has no duplicate event or commit
receipt. On 2026-07-24 it passed against candidate build
`pi-acceptance-20260724-epoch-cache` on a Raspberry Pi 5 Model B running
Ubuntu 24.04.4 / Linux ARM64 under the isolated
`agentbridge-device-agent-acceptance.service`; the production
`agentbridge.service` and backup timer remained active. The accepted task was
`task-oScHMXqCB8cZQexc`, the reconnect resumed after cursor 547 with four
observed events and no duplicates, and the unique commit receipt was
`commit-3kPNLt76wI9DNFo-` for `9abc49e6873fd6a5ac781fa932140132cf4b3f29` on
`refs/heads/staging`. The owner-only attestation was emitted only after these
checks passed.

The final transport/reconnect acceptance used the isolated `/usr/bin/true`
verification profile so the bounded command-reply and receipt-replay evidence
was not coupled to a long-running dependency install. Earlier runs of the
configured Pi verification profile returned `passed=true` from the Pi handler,
but the exploratory curl client canceled one long replay before its reply;
long-running verification remains a release/soak risk rather than a reason to
weaken the local authority contract. Release-signing and independent
publication of hardware evidence remain later release work.

The SQLite routing boundary also updates assignment state and the task's local
event cursor atomically with device fencing/unreachable transitions and event
writes. Remote accepted work is persisted as a fenced command record and the
authenticated replay endpoint re-drives it after reconnect; the controller
queue and restart persistence tests passed. No additional authority or Desktop
datastore was introduced. The signed device link also carries a read-only
typed observation request. The Pi-side handler returns ordered shadow events
and task-scoped real pending approvals; the controller ingests stable remote
event IDs and approval records into local SQLite idempotently, and a resolved
controller approval is not resurrected by a later poll. The local API therefore
exposes device evidence without opening the Pi database or inventing a Desktop
approval identifier. A revoked device ID can be re-enrolled only through a
fresh signed proof and a new key/connection epoch; old task assignments remain
fenced, and cross-task approval-ID/payload collisions fail closed. Remote
event replay is likewise idempotent only for an exact durable match; same-ID
type, revision, timestamp, or payload changes fail closed as an idempotency
conflict.

Remote observation batches now apply provider events, pending approvals, the
task-revision/assignment fence, and the remote observation cursor in one
SQLite transaction. A conflicting event or reassignment rolls back the whole
batch. The Pi agent admits the signed replay cursor before consulting its
durable reply cache, so an old frame is rejected while a fresh-sequence retry
can replay the exact accepted result without invoking the handler twice.
Its bounded observation response advances the remote cursor only through the
last event actually returned; the controller rejects a new remote cursor gap
instead of silently skipping evidence.

Remote commands that are queued while a device is unreachable now also persist
their initial API response under the original request hash. Reconnect replay
bypasses that stale queued response while preserving the same durable command
ID, then advances the idempotency record to the completed response; repeating
the original API request therefore cannot append another queue event or invoke
the device twice.
The Desktop observation loop invokes the authenticated replay endpoint for a
non-local task target before polling events, so a reconnected Pi consumes
accepted work without receiving provider or repository authority.
When reachability recovery advances the device connection epoch and fences an
existing task assignment, the Desktop exposes a reconnect-target action for a
non-completed remote task; it calls the authenticated assignment mutation and
then the same durable replay boundary.

The software transport gate also covers the real TLS WebSocket boundary rather
than only an in-memory link: a paired device is driven through the full
Create → Start → Observe → Approve → Verify → Commit slice using the same
short-lived WSS link model as production. The test forces a disconnect before
Verify, queues the durable command, reconnects with the persisted SQLite
message/sequence counters, and proves exactly one Verify/Commit result without
invoking the controller's local kernel. This is still distinct from the
required ARM64 Linux/systemd and physical reconnect acceptance.
Controller restart recovery also reuses durable approval-resolution and
verification-passed evidence when the process stops after writing that
evidence but before completing the remote command row; replay closes the
original command without invoking the native operation or appending a second
local event. Checkpointed Commit recovery follows the same durable rule.
The Desktop observation timer also serializes in-flight polls, so a slow WSS
replay cannot merge the same event batch twice into the presentation cursor.

Task creation now commits the task, execution/session lineage, target-device
assignment, runtime/local creation events, and its idempotency response in one
SQLite transaction. The local vertical-slice test replays the same create key
and receives the original canonical task, execution, and session IDs. The
SQLite rollback regression also proves that an idempotency conflict leaves no
task, lineage, or local creation event behind.

Local-control task rows now carry a durable `controller_owner` fence. A
`local_control` task cannot be claimed by the co-hosted standalone Telegram
reconciler or worker after restart; standalone compatibility tasks retain the
`standalone` owner, and the local API rejects standalone-owned tasks before
resolving context or invoking an executor. This is tested at the SQLite
creation boundary and by both controller-direction regressions.

Local-control event cursors are split explicitly: the API keeps a global
`cursor` for durable replay queries, while each task event also carries a
contiguous `task_cursor`. Migration 016 backfills that sequence for existing
v2 rows, migration 017 persists the remote Pi observation cursor on the task
assignment, and migration 018 persists controller ownership for existing and
new v2 tasks. The SQLite/projection regression gate proves an event
belonging to another task cannot produce a false Obsidian cursor gap, and the
remote observation test proves the second poll resumes from the accepted
remote cursor.

The auth recovery tests also assert that a successful provider login does not
implicitly resume affected tasks; each task requires the explicit validated
`ResumeTask` action. The standalone controller now waits for an in-flight
provider `Start`/`Resume` registration before an auth suspension returns, so a
concurrent auth check cannot miss a just-created session. The v2 backup command
retains its verified snapshot before applying pinned/state/path-safe event,
attachment, worktree, and backup retention.

Paired-device enrollment rejects non-WSS endpoints at the API boundary so an
unsupported HTTPS configuration cannot reach the signed WebSocket adapter as a
late execution-time failure.

The Desktop device card now starts the one-time pairing challenge and accepts
only a proof whose public-key-derived fingerprint the operator explicitly
confirms. `agentbridge pair device` produces that proof on the headless Pi,
pins the controller public key owner-only, and never emits the private device
key. The command proof path is covered by `TestDevicePairCommandEmitsProofAndPinsControllerKey`.

Task edits now carry the canonical base revision both in the typed request and,
through the Desktop/Obsidian proxy, as a strong `If-Match: "N"` precondition;
the local API rejects malformed or conflicting header revisions before the
authority mutation.

The controlled preflight and acceptance ran with
`RUN_PI_SMOKE=1`/`RUN_PI_ACCEPTANCE=1` against the exact Linux/ARM64 binary;
the immutable candidate manifest and owner-only preflight/acceptance records
were verified by SHA-256 and job nonce. `make proto-check` passed on the
follow-up local run with a temporary Buf 1.50.0 executable and cache staged
under `/tmp`; no generated protocol files were changed.
