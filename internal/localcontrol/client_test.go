package localcontrol_test

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/store/sqlite"
)

func TestLocalClientUsesTypedAuthenticatedBoundary(t *testing.T) {
	data, err := sqlite.OpenV2Runtime(context.Background(), filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	service, err := localcontrol.New(localcontrol.Config{Store: data, Runtimes: fakeCatalog{}, NewID: deterministicIDs()})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("01234567890123456789012345678901")
	handler, err := localcontrol.NewHTTPHandler(service, secret)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := localcontrol.NewClient(server.URL, secret, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	project, err := client.CreateProject(context.Background(), localcontrol.CreateProjectRequest{Name: "Desktop project", IdempotencyKey: "desktop-project"})
	if err != nil || project.Project.ID == "" {
		t.Fatalf("project = %#v err=%v", project, err)
	}
	replayed, err := client.ReplayDeviceCommands(context.Background(), localcontrol.ReplayDeviceCommandsRequest{DeviceID: localcontrol.LocalDeviceID})
	if err != nil || replayed.DeviceID != localcontrol.LocalDeviceID || len(replayed.Pending) != 0 {
		t.Fatalf("replay response = %#v err=%v", replayed, err)
	}
}
