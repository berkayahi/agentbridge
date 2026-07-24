#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

cache_dir=${GOCACHE:-/tmp/agentbridge-gocache}
mkdir -p "$cache_dir"

test ! -d internal/task
test ! -d internal/app
if rg 'sqlite\.Open\(|LegacyStore|github\.com/berkayahi/agentbridge/internal/task/|github\.com/berkayahi/agentbridge/internal/app/' \
	cmd/agentbridge internal/controller/standalone internal/localcontrol --glob '*.go' --glob '!**/*_test.go'; then
	echo "legacy runtime composition remains" >&2
	exit 1
fi
if rg 'demoTask|demoEvents|event-demo-' desktop/app.js; then
	echo "Desktop demo fallback remains" >&2
	exit 1
fi
external_style_urls=$(rg -n 'https?://' desktop/styles.css | rg -v 'data:image' || true)
if rg '@import' desktop/styles.css || [[ -n "$external_style_urls" ]]; then
	echo "Desktop stylesheet has an external dependency" >&2
	exit 1
fi

GOCACHE="$cache_dir" go test ./internal/localcontrol ./internal/obsidian ./internal/store/sqlite \
	-run 'Test(Local|PendingApprovals|RemoteObservation|RemoteCommandQueues|DeviceCommandQueue|PairedDeviceSelectionRotationAndFence|Pi|LinkedRuntime|FencedLink|Projection|OpenV2|V2RuntimeLock|RuntimeStore|API|Transport|Client|ProcessRestartRecovery)' \
	-count=1

echo "local MVP focused gate passed"
