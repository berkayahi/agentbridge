package managed

import (
	"context"
	"errors"
	"time"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
)

type TransportFactory func(context.Context) (Transport, error)
type HandshakeFactory func() (Handshake, error)

type ClientConfig struct {
	TransportFactory      TransportFactory
	Guard                 *ReplayGuard
	Trust                 TrustSet
	Dispatch              Dispatcher
	Backoff               Backoff
	Clock                 func() time.Time
	LocalHandshake        Handshake
	LocalHandshakeFactory HandshakeFactory
}

type Client struct {
	config ClientConfig
}

func NewClient(config ClientConfig) (*Client, error) {
	if config.TransportFactory == nil || config.Guard == nil {
		return nil, ErrTransportClosed
	}
	if err := config.Trust.Validate(); err != nil {
		return nil, err
	}
	if config.LocalHandshakeFactory == nil {
		if err := validateHandshake(config.LocalHandshake); err != nil {
			return nil, err
		}
	}
	if config.Dispatch.Handlers == nil {
		return nil, ErrUnknownCommand
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	return &Client{config: config}, nil
}

func (c *Client) Run(ctx context.Context) error {
	attempt := 0
	for {
		var err error
		localHandshake := c.config.LocalHandshake
		if c.config.LocalHandshakeFactory != nil {
			localHandshake, err = c.config.LocalHandshakeFactory()
			if err != nil {
				return err
			}
		}
		if err := validateHandshake(localHandshake); err != nil {
			return err
		}
		transport, err := c.config.TransportFactory(ctx)
		if err == nil {
			connection, connectionErr := NewConnectionWithOptions(transport, c.config.Guard, c.config.Trust, c.config.Dispatch, ConnectionOptions{
				LocalHandshake: localHandshake, RequireHandshake: true, Clock: c.config.Clock,
			})
			if connectionErr == nil {
				err = connection.Run(ctx)
			} else {
				err = connectionErr
			}
			if transport != nil {
				_ = transport.Close()
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrRevoked) || errors.Is(err, ErrTrustRollback) || errors.Is(err, ErrUntrustedCommand) || errors.Is(err, ErrInvalidFrame) || errors.Is(err, ErrHandshakeRequired) || errors.Is(err, ErrInvalidHandshakeSignature) || errors.Is(err, ErrExpiredFrame) || errors.Is(err, ErrUnknownPayloadType) || errors.Is(err, ErrInvalidFramePayload) {
			return err
		}
		attempt++
		timer := time.NewTimer(c.config.Backoff.Duration(attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

type IdentityClientConfig struct {
	TransportFactory TransportFactory
	Guard            *ReplayGuard
	Trust            TrustSet
	Dispatch         Dispatcher
	Backoff          Backoff
	Clock            func() time.Time
	Identity         deviceidentity.Key
	OrganizationID   string
	DeviceID         string
	ConnectionEpoch  uint64
	Capabilities     []string
}

func NewClientWithIdentity(config IdentityClientConfig) (*Client, error) {
	if !config.Identity.HasPrivate() || config.OrganizationID == "" || config.DeviceID == "" || config.ConnectionEpoch == 0 {
		return nil, ErrInvalidHandshakeSignature
	}
	if err := config.Trust.Validate(); err != nil {
		return nil, err
	}
	handshake, err := SignHandshake(Handshake{
		Major: ProtocolMajor, Minor: ProtocolMinor, OrganizationID: config.OrganizationID,
		DeviceID: config.DeviceID, ConnectionEpoch: config.ConnectionEpoch,
		ControllerEpoch: config.Trust.HighestEpoch, Capabilities: append([]string(nil), config.Capabilities...),
	}, config.Identity)
	if err != nil {
		return nil, err
	}
	return NewClient(ClientConfig{
		TransportFactory: config.TransportFactory, Guard: config.Guard, Trust: config.Trust,
		Dispatch: config.Dispatch, Backoff: config.Backoff, Clock: config.Clock,
		LocalHandshake: handshake,
	})
}

type PersistentClientConfig struct {
	State          *FileStateStore
	WebSocket      WebSocketConfig
	Identity       deviceidentity.Key
	Enrollment     *deviceidentity.EnrollmentRecord
	OrganizationID string
	DeviceID       string
	Capabilities   []string
	Dispatch       Dispatcher
	Backoff        Backoff
	Clock          func() time.Time
}

// NewPersistentClient composes the durable replay/inbox state, enrolled
// device identity, persisted platform trust, and outbound WSS transport.
func NewPersistentClient(config PersistentClientConfig) (*Client, error) {
	if config.State == nil || !config.Identity.HasPrivate() || config.OrganizationID == "" || config.DeviceID == "" {
		return nil, ErrInvalidHandshakeSignature
	}
	if config.Enrollment != nil {
		if config.Enrollment.OrganizationID != config.OrganizationID || config.Enrollment.DeviceID != config.DeviceID || config.Enrollment.Fingerprint != config.Identity.Fingerprint() {
			return nil, ErrInvalidHandshakeSignature
		}
		if config.Enrollment.Revoked || config.Enrollment.Quarantined {
			return nil, ErrRevoked
		}
	}
	if err := ValidateWebSocketURL(config.WebSocket.URL); err != nil {
		return nil, err
	}
	trust, err := config.State.LoadTrust(context.Background())
	if err != nil {
		return nil, err
	}
	if err := trust.Validate(); err != nil {
		if config.Enrollment == nil {
			return nil, err
		}
		trust, err = TrustSetFromEnrollment(*config.Enrollment)
		if err != nil {
			return nil, err
		}
		if err := config.State.SaveTrust(context.Background(), trust); err != nil {
			return nil, err
		}
	}
	guard, err := NewReplayGuardWithInbox(config.State, config.OrganizationID, config.DeviceID)
	if err != nil {
		return nil, err
	}
	return NewClient(ClientConfig{
		TransportFactory: func(ctx context.Context) (Transport, error) {
			return NewWebSocketTransport(ctx, config.WebSocket)
		},
		Guard: guard, Trust: trust, Dispatch: config.Dispatch, Backoff: config.Backoff, Clock: config.Clock,
		LocalHandshakeFactory: func() (Handshake, error) {
			epoch, err := config.State.NextConnectionEpoch(context.Background())
			if err != nil {
				return Handshake{}, err
			}
			return SignHandshake(Handshake{
				Major: ProtocolMajor, Minor: ProtocolMinor, OrganizationID: config.OrganizationID,
				DeviceID: config.DeviceID, ConnectionEpoch: epoch, ControllerEpoch: trust.HighestEpoch,
				Capabilities: append([]string(nil), config.Capabilities...),
			}, config.Identity)
		},
	})
}
