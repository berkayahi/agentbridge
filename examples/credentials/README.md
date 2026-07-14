# Credential source files

Create credential source files only in the deployment account's private
configuration directory. Do not place them in this repository.

The supplied systemd unit loads `telegram_bot_token`. Its source file must
contain only the bot credential, have mode `0600`, and live in a mode `0700`
directory. AgentBridge reads the service-private copy from systemd's
credentials directory.

Codex CLI and Claude Code each own their subscription session files. Neither
provider uses an AgentBridge credential source file.
