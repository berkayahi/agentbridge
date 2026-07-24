package localcontrol

import "testing"

func TestValidDeviceEndpointRequiresWSS(t *testing.T) {
	for endpoint, want := range map[string]bool{
		"wss://pi.local/agentbridge":           true,
		"":                                     false,
		"https://pi.local/agentbridge":         false,
		"ws://pi.local/agentbridge":            false,
		"wss://pi.local/agentbridge#fragment":  false,
		"wss://user:pass@pi.local/agentbridge": false,
		"pi.local/agentbridge":                 false,
	} {
		if got := validDeviceEndpoint(endpoint); got != want {
			t.Fatalf("validDeviceEndpoint(%q) = %v, want %v", endpoint, got, want)
		}
	}
}
