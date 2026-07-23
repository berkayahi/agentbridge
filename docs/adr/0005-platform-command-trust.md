# ADR 0005: Platform command trust bootstrap

Status: Accepted

Device update signing and platform command signing are separate trust
domains. Enrollment pins the active command signer set, a next set, and the
highest accepted controller epoch. A command is accepted only after its
canonical envelope signature verifies against the active set and its epoch is
strictly non-decreasing. A signer rotation requires overlap and an explicit
signed trust-set update.

The device persists the highest accepted epoch before dispatching a command.
Rollback, freeze, emergency revocation, and a command signed only by an
update signer fail closed. Release signer fingerprints and threshold policy
are kept in `deploy/trust/release-signers.json`; the release scripts never
accept a lookalike or mirror remote.
