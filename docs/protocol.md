# AgentBridge device protocol v1

The nested `protocol` module is the public, independently versioned device
contract. `protocol/VERSION` is the stable semantic base version (`1.0.0`),
while release candidates use the same source commit with an `-rc.N` tag.

Every frame carries organization and device identity, connection/controller
epochs, a monotonic message and stream sequence, causation/correlation IDs,
issued and expiry times, a typed payload digest, signing-key ID, and a
signature. The canonical signed bytes exclude only the signature field. A
receiver rejects missing authorization fields, unknown payloads, expired
frames, stale epochs, oversized payloads, and a major-version mismatch.

Within a major version, additions are optional and enum zero is unspecified.
Capabilities are negotiated before a newly introduced command or event is
used. Removing or reusing a field number is a breaking protocol change and
requires a new major version.

Run `make proto` after installing the pinned Buf plugins. `make proto-check`
must resolve exactly one latest stable protocol tag after the bootstrap
baseline exists; it never permanently skips compatibility checking.
