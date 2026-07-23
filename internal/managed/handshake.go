package managed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var (
	ErrIncompatibleProtocol = errors.New("managed: incompatible protocol")
	ErrHandshakeRequired    = errors.New("managed: signed handshake required")
)

type Handshake struct {
	Major           uint32
	Minor           uint32
	OrganizationID  string
	DeviceID        string
	ConnectionEpoch uint64
	ControllerEpoch uint64
	Capabilities    []string
	SigningKeyID    string
	Signature       []byte
}

type HandshakeTransport interface {
	PerformHandshake(context.Context, Handshake) (Handshake, error)
}

type ConnectionOptions struct {
	LocalHandshake   Handshake
	RequireHandshake bool
	Clock            func() time.Time
	OnReady          ConnectionReady
}

func (h Handshake) CanonicalSigningBytes() ([]byte, error) {
	capabilities := append([]string(nil), h.Capabilities...)
	sort.Strings(capabilities)
	return json.Marshal(struct {
		Major, Minor                     uint32
		OrganizationID, DeviceID         string
		ConnectionEpoch, ControllerEpoch uint64
		Capabilities                     []string
		SigningKeyID                     string
	}{
		Major: h.Major, Minor: h.Minor, OrganizationID: h.OrganizationID, DeviceID: h.DeviceID,
		ConnectionEpoch: h.ConnectionEpoch, ControllerEpoch: h.ControllerEpoch,
		Capabilities: capabilities, SigningKeyID: h.SigningKeyID,
	})
}

func Negotiate(local, remote Handshake) (Handshake, error) {
	if err := validateHandshake(local); err != nil || validateHandshake(remote) != nil {
		return Handshake{}, ErrHandshakeRequired
	}
	if local.Major != remote.Major {
		return Handshake{}, ErrIncompatibleProtocol
	}
	if local.OrganizationID == "" || local.OrganizationID != remote.OrganizationID || local.DeviceID == "" || remote.DeviceID != local.DeviceID {
		return Handshake{}, errors.New("managed: handshake identity mismatch")
	}
	if remote.ConnectionEpoch == 0 || remote.ControllerEpoch == 0 {
		return Handshake{}, errors.New("managed: handshake epoch missing")
	}
	minor := local.Minor
	if remote.Minor < minor {
		minor = remote.Minor
	}
	return Handshake{Major: local.Major, Minor: minor, OrganizationID: local.OrganizationID, DeviceID: local.DeviceID, ConnectionEpoch: remote.ConnectionEpoch, ControllerEpoch: remote.ControllerEpoch, Capabilities: intersectStrings(local.Capabilities, remote.Capabilities)}, nil
}

func (h Handshake) String() string {
	return fmt.Sprintf("%d.%d/%s/%s/%d/%d", h.Major, h.Minor, h.OrganizationID, h.DeviceID, h.ConnectionEpoch, h.ControllerEpoch)
}

func intersectStrings(left, right []string) []string {
	set := make(map[string]struct{}, len(right))
	for _, value := range right {
		set[value] = struct{}{}
	}
	result := make([]string, 0, len(left))
	for _, value := range left {
		if _, ok := set[value]; ok {
			result = append(result, value)
		}
	}
	return result
}

func validateHandshake(h Handshake) error {
	if h.Major == 0 || strings.TrimSpace(h.OrganizationID) == "" || strings.TrimSpace(h.DeviceID) == "" || h.ConnectionEpoch == 0 || h.ControllerEpoch == 0 || strings.TrimSpace(h.SigningKeyID) == "" || len(h.Signature) == 0 {
		return ErrHandshakeRequired
	}
	return nil
}
