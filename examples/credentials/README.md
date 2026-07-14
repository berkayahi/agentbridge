# Credential source files

Create credential source files only in the deployment account's private
configuration directory. Do not place them in this repository.

The supplied systemd unit loads `telegram_bot_token` and
`claude_oauth_token`. Each source file must contain only its credential, have
mode `0600`, and live in a mode `0700` directory. AgentBridge reads the
service-private copies from systemd's credentials directory.

Codex CLI owns its ChatGPT subscription session files and does not use an
AgentBridge credential source file.
