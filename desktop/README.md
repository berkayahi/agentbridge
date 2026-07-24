# Kovan Desktop surface

This is the thin presentation surface for the local-first Kovan control plane.
It never opens AgentBridge SQLite, chooses a repository filesystem path, or
spawns Claude/Codex. `host.js` is the dependency-free native-host boundary: it
serves this surface on loopback, keeps the local API secret in the host, and
proxies only authenticated requests to the owner-only AgentBridge Unix socket.
An Electron/Tauri shell can later replace the host without changing the UI
contract. The browser fallback expects the same `/v1` API on its origin.

Run the local host with absolute paths:

```sh
node desktop/host.js --socket /absolute/path/run/local-api.sock --secret /absolute/path/run/local-api.secret
```

The host prints an ephemeral loopback URL. It never puts the AgentBridge
secret into browser JavaScript or a URL.

The current surface covers workspace setup, task creation, status/action
buttons, task-scoped provider approval discovery, durable event cursors, and
the Create → Start → Observe → Approve/Cancel → Verify → Commit loop. Approval
actions use the real pending approval ID returned by AgentBridge rather than a
presentation-side placeholder. It ships as dependency-free HTML/CSS/JS
so a future native shell can bundle it without introducing a second authority.
The stylesheet uses local system-font fallbacks and has no CDN asset dependency,
so the surface remains usable while the controller is offline.

When a Pi is paired, the local API returns it as a selectable execution
device. Pairing also exposes the owner-only controller public key in the
one-time challenge so the headless device can verify signed commands; the
Desktop surface still sends only API mutations and never handles that key or
the Pi transport directly.

The Desktop device card now drives the local pairing ceremony. Create the
one-time challenge, save the copied JSON as an owner-only file on the Pi, then
run the same binary there:

```sh
agentbridge pair device \
  --challenge /srv/agentbridge/pairing-challenge.json \
  --data-dir /srv/agentbridge \
  --name "Build Pi" \
  --endpoint wss://build-pi.tailnet/agentbridge \
  --output /srv/agentbridge/pairing-proof.json
```

The command creates or reuses the owner-only device key, pins the controller
public key without silently replacing an existing one, and emits a proof JSON
that can be pasted back into Desktop. Desktop computes and displays the
device-key fingerprint and requires an explicit operator confirmation before
calling `POST /v1/devices/pair`. A proof contains no private key.

The device manager also exposes the authenticated lifecycle controls: mark a
paired device unreachable/reachable, revoke it (which fences existing task
assignments), and rotate its public key after the new key is installed on the
device. Key rotation requires fingerprint confirmation and advances the
connection epoch; a revoked device must return through a fresh pairing
challenge.

When a transport failure fences an existing remote task assignment, marking the
device reachable exposes a `reconnect target` action on the non-completed remote
task. That action uses the authenticated task-device mutation to bind the task
to the new connection epoch, then invokes the durable device replay boundary;
the Desktop still never receives provider or repository authority.
