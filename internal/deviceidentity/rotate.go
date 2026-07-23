package deviceidentity

import "errors"

var ErrRotationUnavailable = errors.New("device identity: rotation requires enrollment")

type Rotation struct {
	PreviousFingerprint string
	Next                Key
}

func Rotate(current Key) (Rotation, error) {
	if !current.HasPrivate() {
		return Rotation{}, ErrRotationUnavailable
	}
	next, err := Generate()
	if err != nil {
		return Rotation{}, err
	}
	return Rotation{PreviousFingerprint: current.Fingerprint(), Next: next}, nil
}
