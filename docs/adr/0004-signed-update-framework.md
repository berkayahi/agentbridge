# ADR 0004: Signed update framework

Status: Accepted

AgentBridge updates are accepted only when signed metadata verifies against a
separate update trust root and a threshold. The immutable binary identity is
the tuple `(ProductVersion, BuildTag, source commit, artifact digest)` plus
target platform. ProductVersion is core SemVer without prerelease or build
metadata; BuildTag records the exact signed candidate tag.

The device stores its highest accepted metadata version and last-known-good
identity in a secure floor that ordinary database/archive restore cannot lower.
Installation stages and verifies a target, atomically swaps it, runs the
bounded health contract, and restores the previous binary on failure. Platform
command trust and update trust are never interchangeable.
