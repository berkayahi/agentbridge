# Managed mode

AgentBridge has two explicit authority modes:

- `standalone`: local Telegram/dashboard adapters submit work to the local
  execution kernel.
- `managed`: the enrolled controller is the canonical source of commands and
  task identity. Local surfaces provide diagnostics, emergency cancellation,
  required confirmations, and recovery only.

The device key is generated locally and stored owner-only. Enrollment binds a
one-time claim to the organization, device, browser-confirmed fingerprint,
public key, trust-set digest, nonce, and expiry. A successful proof writes only
public enrollment facts beside the key. The key is never included in an
ordinary backup.

Managed frames use protocol-compatible canonical signing bytes. The device
checks organization/device identity, protocol version, epochs, message and
sequence monotonicity, expiry, payload digest, and the platform command trust
set before admitting a command. The durable managed state records the inbox
entry and cursor together before dispatch, so a restart cannot turn a replayed
command into a second provider or Git side effect.

Mode activation is durable and cannot change while the mode store records an
active execution. Re-enrollment is required when the OS-protected key is
missing or does not match the enrollment record, when the identity is revoked
or quarantined, or when a restored archive comes from another machine.
