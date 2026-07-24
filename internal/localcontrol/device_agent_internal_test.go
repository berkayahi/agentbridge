package localcontrol

import "testing"

func TestDeviceResultKeyScopesConnectionEpoch(t *testing.T) {
	if first, second := deviceResultKey(1, "verify:task-1:3"), deviceResultKey(2, "verify:task-1:3"); first == second {
		t.Fatalf("result keys must be fenced by connection epoch: %q", first)
	}
	if got := deviceResultKey(4, "verify:task-1:3"); got != "4:verify:task-1:3" {
		t.Fatalf("result key = %q", got)
	}
}
