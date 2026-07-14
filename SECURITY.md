# Security Policy

## Supported versions

Until the first stable release, security fixes are provided on the latest
commit of the default branch. Releases will document any broader support
window.

## Reporting a vulnerability

Use GitHub's **Report a vulnerability** private security-advisory flow for this
repository. Include affected versions, prerequisites, impact, and minimal
reproduction steps. Do not open a public issue with exploit details, tokens,
private repository content, Telegram data, or CLI session files.

If private reporting is temporarily unavailable, open a public issue that asks
the maintainers to enable a private contact channel without including any
vulnerability details.

Expect an acknowledgement within seven days. We will coordinate validation,
fix preparation, disclosure timing, and credit with the reporter.

## Deployment responsibility

AgentBridge runs development tools with the operator's local permissions. Keep
the dashboard on loopback behind private tailnet access, restrict Telegram by
numeric identity and private chat, use systemd credentials, and configure exact
repository delivery refs. Provider authentication is delegated to the official
subscription-authenticated CLIs. Never add provider API-key credentials.
