# ADR 0006: Artifact envelope profile

Status: Accepted

Artifacts use an envelope with a stable manifest, plaintext digest, encrypted
payload digest, media type, size, and profile identifier. The profile uses a
standard authenticated encryption construction selected by the managed
implementation; it does not define custom cryptography. Chunks are ordered by
artifact ID and offset, and a receipt is accepted only after the final digest
matches the manifest.

`protocol/fixtures/v1/artifact-envelope.json` is the language-neutral shape
fixture. It contains no customer data or key material.
