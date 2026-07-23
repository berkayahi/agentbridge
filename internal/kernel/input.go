package kernel

import "strings"

// Input is the transport-neutral user input accepted by the execution kernel.
// Provider-specific message and transcript types do not cross this boundary.
type Input struct{ Text string }

func (i Input) Validate() error {
	if strings.TrimSpace(i.Text) == "" {
		return ErrInvalidCommand
	}
	return nil
}
