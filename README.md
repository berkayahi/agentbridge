# AgentBridge

AgentBridge is an early-stage, public open-source bridge for operating
supported AI agent CLIs from remote interfaces. The project is intended to remain generic:
deployments supply their own providers, repositories, identities, and secrets
through configuration rather than source-code changes.

AgentBridge delegates authentication and execution to supported official CLIs.
Authentication methods, entitlements, usage limits, and any charges are
governed by the installed CLI and its provider. Consult each provider's current
terms and CLI documentation before deployment.

## Development

AgentBridge requires Go 1.26.4 or newer.

```sh
make test
make lint
make build
./bin/agentbridge version
```

The repository is under active development and is not ready for production
use. Issues and focused pull requests are welcome. Never commit tokens,
credentials, deployment identities, or private repository details.

## Documentation

- [Architecture](docs/architecture.md)
- [Raspberry Pi operations](docs/operations.md)
- [Subscription authentication recovery](docs/auth-recovery.md)
- [Safe upgrade and rollback](docs/upgrade.md)
- [Incident response](docs/incident-response.md)
- [Contributing](CONTRIBUTING.md) and [security reporting](SECURITY.md)

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
