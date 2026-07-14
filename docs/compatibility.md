# CLI compatibility

AgentBridge uses official subscription-authenticated CLIs and does not call model-provider APIs.
CLI paths remain runtime-configurable so operators can upgrade independently.

| Integration | Validated version | Validation contract |
|---|---:|---|
| Codex CLI app server | 0.143.0 | Generated v2 schema, initialize handshake, thread/turn lifecycle, approvals, account usage |
| Claude Code stream JSON | 2.1.176 | Stream JSON session lifecycle and status-line input |

## Upgrade procedure

1. Install the candidate official CLI in an isolated environment.
2. Regenerate or export its protocol schemas when the CLI supports it.
3. Compare methods and required fields against the small contracts and sanitized fixtures in this repository.
4. Run provider race tests and a subscription-authenticated smoke test without API-key environment variables.
5. Update this table only after the checks pass. AgentBridge does not pin or install CLI binaries.
