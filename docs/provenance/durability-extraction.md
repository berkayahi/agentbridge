# Durability extraction provenance

Task 8 reimplements the selected durability semantics from archived donor
objects `6fd983f8612ca8b5c45dfab10e4fafbff5761112` and
`d5f86c542c0b248e80f439f093f5d9829ff240ee`:

- durable intent ownership uses an expiring lease and a fencing owner;
- provider failures distinguish safe pre-side-effect retry from unknown outcome;
- retry delay is deterministic for an intent and attempt so a restart cannot
  change its schedule.

The public implementation excludes donor migrations, Telegram payloads, old
aggregate records, and any blind replay of a provider turn. Unknown external
outcomes stop at `reconciliation_required` until a provider-native identity or
receipt can prove the effect.
