# Events and encrypted artifacts

Raw terminal output is diagnostic data, not a normal structured event. It is
off by default and must be enabled by a policy with a byte quota and expiry.
The egress guard redacts credentials and local secret paths before any upload;
a finding quarantines the payload.

Artifact uploads require a signed immutable grant binding organization, device,
execution, object key, artifact ID, algorithm/key ID, size/type/hash, policy
digest, expiry, and one-use nonce. The public client uses the standard
AES-256-GCM envelope profile, uploads ciphertext only, enforces immutable
object identity and ordered chunks, and emits a receipt after finalization.
