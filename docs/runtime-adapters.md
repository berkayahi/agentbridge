# Runtime adapters

AgentBridge domain identifiers live in `internal/workmodel`, while provider
adapters translate the official Codex and Claude CLI protocols into opaque
runtime IDs. Telegram, the local dashboard, and managed WSS are transport
adapters; they do not own task state, provider sessions, repository paths, or
authorization decisions.

The standalone adapter keeps local operator projection concerns isolated from
the kernel contracts. Managed mode uses the signed controller adapter and
cannot create canonical work from a device-local presentation surface.
The shared standalone composition persists task controller ownership: local API
tasks are marked `local_control` and are never claimed by the Telegram adapter's
restart reconciliation or worker, while transport-neutral standalone tasks are
marked `standalone`. The local API likewise rejects a standalone-owned task
before it resolves context or invokes an execution adapter.

Local Pi execution uses the same typed boundary: the controller opens only the
paired `wss://` endpoint, signs a high-level `DeviceCommand`, and accepts a
`DeviceReply` only when the pairing public key, task command correlation, and
connection epoch all match. `FencedLink` owns live-link replay/conflict
handling; SQLite idempotency remains the durable local authority. A Pi never
receives a provider executable, repository path, credential, or Desktop UI.
The Pi-side `DeviceAgent` verifies the controller handshake and signed typed
frames before dispatch, and its WSS handler is TLS-only. Controller
message/sequence counters are reserved per device in the local SQLite
authority, while the Pi result cache and replay cursor survive a headless
process restart.
The agent advances its signed replay cursor before reading that cache, which
keeps an old frame from becoming a reusable authorization token while allowing
a new-sequence retry to receive the persisted reply.

The same `agentbridge serve` binary can host the headless processor when a
standalone configuration enables `device_agent`. The device configuration
supplies the owner-only device key, the controller public key copied from the
one-time pairing challenge, a TLS certificate/key, and owner-only replay/result
state paths:

```yaml
device_agent:
  enabled: true
  listen: 0.0.0.0:8788
  organization_id: local
  device_id: build-pi
  identity_path: /home/operator/.local/share/agentbridge/device-key.json
  controller_public_key_path: /home/operator/.local/share/agentbridge/controller.pub
  tls_cert_path: /home/operator/.local/share/agentbridge/device.crt
  tls_key_path: /home/operator/.local/share/agentbridge/device.key
  results_path: /home/operator/.local/share/agentbridge/device-results.json
  replay_state_path: /home/operator/.local/share/agentbridge/device-replay.json
  connection_epoch: 1
  controller_epoch: 1
```

The agent uses the configured provider and repository profiles on that host;
the controller sends only signed typed execution context and action payloads.
The local v2 task row on the device is an execution/restart projection, not a
second Desktop authority. Install `agentbridge-device-agent.service` for this
mode; it intentionally has no `LoadCredential` entry for Telegram and starts
the same exact binary without the standalone owner API. The controlled
ARM64/systemd smoke remains the release gate.
