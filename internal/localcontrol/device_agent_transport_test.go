package localcontrol_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
	"github.com/berkayahi/agentbridge/internal/localcontrol"
	"github.com/berkayahi/agentbridge/internal/managed"
)

func TestPiDeviceAgentServesSignedWSSRequestResponse(t *testing.T) {
	controller, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	pi, err := deviceidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000, 0).UTC()
	agent, err := localcontrol.NewDeviceAgent(localcontrol.DeviceAgentConfig{
		Identity: pi, ControllerPublicKey: controller.PublicKey(), OrganizationID: "local", DeviceID: "pi-one",
		ConnectionEpoch: 1, ControllerEpoch: 1, Handler: func(context.Context, localcontrol.DeviceCommand) (localcontrol.DeviceReply, error) {
			return localcontrol.DeviceReply{Accepted: true, Payload: json.RawMessage(`{"served":"wss"}`)}, nil
		}, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := localcontrol.NewDeviceAgentWebSocketHandler(agent, managed.MaxFrameBytes, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	endpoint := "wss" + strings.TrimPrefix(server.URL, "https")
	link, err := localcontrol.NewWebSocketDeviceLink(context.Background(), localcontrol.WebSocketDeviceLinkConfig{
		Identity: controller, PeerPublicKey: pi.PublicKey(), OrganizationID: "local", DeviceID: "pi-one",
		ConnectionEpoch: 1, ControllerEpoch: 1, Endpoint: endpoint,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool},
		Clock:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()
	reply, err := link.Execute(context.Background(), localcontrol.DeviceCommand{
		ID: "start:tls", Operation: "start", TaskID: "task-1", DeviceID: "pi-one", ConnectionEpoch: 1,
		Payload: json.RawMessage(`{"input":"run"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reply.Accepted || string(reply.Payload) != `{"served":"wss"}` {
		t.Fatalf("WSS reply = %#v", reply)
	}
}
