package managed

import (
	"errors"
	"fmt"
)

var ErrIncompatibleProtocol = errors.New("managed: incompatible protocol")

type Handshake struct {
	Major           uint32
	Minor           uint32
	OrganizationID  string
	DeviceID        string
	ConnectionEpoch uint64
	ControllerEpoch uint64
	Capabilities    []string
}

func Negotiate(local, remote Handshake) (Handshake, error) {
	if local.Major == 0 || local.Major != remote.Major {
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
