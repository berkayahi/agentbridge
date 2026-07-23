# ADR 0003: Recovery HPKE profile

Status: Accepted

Recovery payloads use the standard HPKE construction selected by the managed
protocol implementation and are represented by the language-neutral recovery
fixture in `protocol/fixtures/v1/recovery-hpke.json`. AgentBridge does not
invent a cipher, KDF, or AEAD. The recipient device key, provider context,
request ID, and expiry are bound as authenticated context; ciphertext and
evidence digests are carried in typed protocol messages.

The fixture is an interoperability vector, not a production secret. Private
provider credentials never appear in protocol frames or fixtures.
