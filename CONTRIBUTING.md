# Contributing to AgentBridge

Thank you for helping make remote agent operations safer and more useful.

## Development workflow

1. Use Go 1.26.4 or newer.
2. Fork the repository and create a focused branch.
3. Add a failing test before changing behavior.
4. Run `go test -race ./...`, `go vet ./...`, and `bash -n deploy/*.sh scripts/*.sh`.
5. Use an English [Conventional Commit](https://www.conventionalcommits.org/) message.
6. Open a pull request that explains risk, verification, and documentation changes.

Keep provider and repository behavior generic. Tests must use disposable
repositories, fake subprocesses, and example identities. Never commit tokens,
CLI session data, private repository information, deployment addresses, or
screenshots containing private data.

Provider integration tests must not spend quota or make model turns. Real CLI
acceptance tests belong to a maintainer-controlled, explicit test procedure.

## Scope

Focused bug fixes, tests, documentation, provider compatibility improvements,
and secure operational tooling are welcome. Discuss large protocol or storage
changes in an issue before implementation.

By participating, you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md).
Report security issues through the private process in [SECURITY.md](SECURITY.md).
