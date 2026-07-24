# Kovan Obsidian projection/client

This is the real Obsidian host for the standalone and managed projection/client
boundary. It is intentionally a thin client:

- it talks to `desktop/host.js` over loopback HTTP;
- the host keeps the AgentBridge local API secret and proxies to the owner-only
  Unix socket;
- it stores only canonical IDs, revisions, cursors, and sync metadata in notes;
- it never opens SQLite, chooses a repository path, starts a provider, or writes
  credentials/transcripts into Markdown.

For managed mode, set `Mode` to `managed`, provide the HTTPS Control API URL,
and provide the user's bearer token. The adapter sends only typed project,
board, task, and task-event requests; it normalizes the cloud's bare task/event
responses into the same projection envelope used by the local host. Managed
mode never accepts a local API secret or treats Obsidian as authority.

## Install locally

1. Start the Desktop host and copy its printed URL, for example
   `http://127.0.0.1:43127`.
2. Copy `manifest.json` and `main.js` into
   `<vault>/.obsidian/plugins/kovan-local/`.
3. Enable **Kovan Local** in Obsidian and set the Desktop host URL plus the
  opaque canonical repository ID in the plugin settings.

For managed mode, use the same plugin and set `Mode`, `Managed Control URL`,
and `Managed bearer token` instead. The managed client requires HTTPS except
for loopback HTTP used by tests.

The plugin bootstraps a short-lived in-memory session cookie from the host. It
does not read the local API secret file. If the host restarts, the next request
obtains a new session.

## Commands

- **Sync active Kovan task** observes ordered events and repairs a cursor gap
  from cursor zero before updating the managed block. The API's global replay
  cursor and the task-scoped contiguous cursor are stored separately, so
  events from other tasks cannot create a false gap.
- **Sync all managed Kovan tasks** performs the same operation for every
  managed Markdown file.
- **Send active Kovan task edits to API** submits only the explicitly marked
  managed task view with its stored base revision, deterministic idempotency
  key, and strong `If-Match: "N"` precondition; personal Markdown remains
  local and stale revisions fail before mutation.
- **Create canonical Kovan task from active note** calls project, board, and
  task APIs first, then writes the returned canonical task ID/revision.
- **Import Kovan templates through the local API** reads the configured local
  template source once, uses stable idempotency keys, and writes the imported
  canonical task as a managed note.

Unmarked notes remain local drafts until the explicit create command. Personal
Markdown outside the managed task view is preserved; a note changed during a
sync or update is not overwritten. The task view is hidden behind versioned
HTML comments so arbitrary personal Markdown is never sent as a task mutation.
