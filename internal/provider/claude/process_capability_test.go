package claude

import (
	"os"
	"testing"

	"github.com/berkayahi/agentbridge/internal/mcpserver"
)

func TestCapabilityFileSupportsIndependentInheritedReaders(t *testing.T) {
	file, err := capabilityFile([]byte("task-capability"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	if _, err := os.Stat(file.Name()); !os.IsNotExist(err) {
		t.Fatalf("capability path remains visible: %v", err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("capability mode = %o, want 600", got)
	}

	// Claude can spawn the MCP server and statusline as separate children. Both
	// inherit FD 3, so neither read may consume the shared open-file offset.
	for _, child := range []string{"mcp", "statusline"} {
		got, err := mcpserver.ReadCapability(int(file.Fd()), nil, nil)
		if err != nil {
			t.Fatalf("%s read: %v", child, err)
		}
		if string(got) != "task-capability" {
			t.Fatalf("%s capability = %q", child, got)
		}
	}
}
