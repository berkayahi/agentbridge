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

## Execution profiles

A task may select a provider-neutral execution profile:

```json
{
  "model": "gpt-5.6-sol",
  "reasoning_effort": "high"
}
```

`model` remains available as a compatibility field on task creation,
`TaskView`, and `StartCommand`. A model-only request is normalized to an
execution profile with an empty `reasoning_effort`. If both `model` and
`execution_profile.model` are supplied, they must match.

The provider catalog's compatibility `models` list contains model IDs.
`model_profiles` is the authoritative capability view: every model carries
only its own `supported_reasoning_efforts`, including the provider-reported
default. Each effort has a `kind` of `reasoning` or `orchestration`. Clients
must not form a model/effort combination that is absent from the same model
entry.

Codex app-server 0.145.0 advertises Ultra through the `reasoningEffort` catalog
field and accepts it through `turn/start.effort`; its older `multiAgentMode`
field is deprecated and ignored. AgentBridge therefore preserves the value
`ultra` as `execution_profile.reasoning_effort` but reports it with
`kind: "orchestration"`. This distinguishes automatic delegation from ordinary
reasoning depth without translating to the ignored field.

The inspected 0.145.0 catalog reports GPT-5.6 Sol and Terra with `low`,
`medium`, `high`, `xhigh`, `max`, and `ultra`; GPT-5.6 Luna reports the first
five and does not report `ultra`. These values are observations, not a compiled
allowlist: AgentBridge reads `model/list` at runtime and publishes only the
models and per-model efforts returned by the installed provider.

Task creation validates the complete profile against the live provider
catalog. The profile is stored on the canonical task, included in
`task_created` events, sent in paired-device execution manifests, and reused
unchanged for initial start and restart/resume. An unsupported combination is
rejected; there is no provider-default fallback for an explicit profile.

Run `make proto` after installing the pinned Buf plugins. `make proto-check`
must resolve exactly one latest stable protocol tag after the bootstrap
baseline exists; it never permanently skips compatibility checking.
