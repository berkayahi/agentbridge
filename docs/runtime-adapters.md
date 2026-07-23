# Runtime adapters

AgentBridge domain identifiers live in `internal/workmodel`, while provider
adapters translate the official Codex and Claude CLI protocols into opaque
runtime IDs. Telegram, the local dashboard, and managed WSS are transport
adapters; they do not own task state, provider sessions, repository paths, or
authorization decisions.

The standalone adapter keeps local operator projection concerns isolated from
the kernel contracts. Managed mode uses the signed controller adapter and
cannot create canonical work from a device-local presentation surface.
