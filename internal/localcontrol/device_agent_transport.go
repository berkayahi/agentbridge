package localcontrol

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/berkayahi/agentbridge/internal/managed"
	"golang.org/x/net/websocket"
)

// Serve accepts one authenticated request/response connection from the
// controller. The caller owns listener lifecycle and should call this once
// per accepted transport; a disconnected Pi can accept the next connection
// without losing typed command results.
func (a *DeviceAgent) Serve(ctx context.Context, transport managed.Transport) error {
	if a == nil || transport == nil || a.replay == nil || a.results == nil || a.handler == nil {
		return ErrDeviceAgentProtocol
	}
	acceptor, ok := transport.(managed.HandshakeAcceptor)
	if !ok {
		return ErrDeviceAgentProtocol
	}
	local, err := managed.SignHandshake(managed.Handshake{
		Major: managed.ProtocolMajor, Minor: managed.ProtocolMinor,
		OrganizationID: a.organizationID, DeviceID: a.deviceID,
		ConnectionEpoch: a.connectionEpoch, ControllerEpoch: a.controllerEpoch,
		Capabilities: []string{deviceLinkCapability},
	}, a.identity)
	if err != nil {
		return err
	}
	remote, err := acceptor.AcceptHandshake(ctx, local)
	if err != nil {
		return err
	}
	if remote.OrganizationID != a.organizationID || remote.DeviceID != a.deviceID || remote.ConnectionEpoch != a.connectionEpoch || remote.ControllerEpoch != a.controllerEpoch || !hasCapability(remote.Capabilities, deviceLinkCapability) {
		return ErrDeviceFence
	}
	if err := managed.VerifyHandshakeSignature(remote, a.controllerKey.PublicKey()); err != nil {
		return ErrDeviceLinkUnauthenticated
	}
	if _, err := managed.Negotiate(local, remote); err != nil {
		return err
	}
	for {
		frame, err := transport.Receive(ctx)
		if err != nil {
			if errors.Is(err, managed.ErrTransportClosed) {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				return managed.ErrTransportClosed
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		reply, err := a.Handle(ctx, frame)
		if err != nil {
			return err
		}
		if err := transport.Send(ctx, reply); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
}

func hasCapability(capabilities []string, wanted string) bool {
	for _, capability := range capabilities {
		if capability == wanted {
			return true
		}
	}
	return false
}

// NewDeviceAgentWebSocketHandler exposes one Pi-side WSS endpoint. TLS is
// configured by the owning HTTP server; signed handshakes provide the device
// authorization boundary in addition to the encrypted transport.
func NewDeviceAgentWebSocketHandler(agent *DeviceAgent, maxMessageBytes int, readPoll time.Duration) (http.Handler, error) {
	if agent == nil {
		return nil, ErrDeviceAgentProtocol
	}
	return websocket.Handler(func(conn *websocket.Conn) {
		ctx := context.Background()
		request := conn.Request()
		if request == nil || request.TLS == nil {
			_ = conn.Close()
			return
		}
		if request.Context() != nil {
			ctx = request.Context()
		}
		transport := managed.NewWebSocketTransportFromConn(conn, maxMessageBytes, readPoll)
		defer transport.Close()
		_ = agent.Serve(ctx, transport)
	}), nil
}

var _ managed.HandshakeAcceptor = (*managed.WebSocketTransport)(nil)
