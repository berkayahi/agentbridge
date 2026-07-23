package sqlite

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

var ErrDatabaseInUse = errors.New("sqlite: database is in use")

// AcquireDatabaseRuntimeLock holds a cooperative process lock for the whole
// lifetime of a daemon or an exclusive database operation. SQLite permits a
// second process to open an idle database, so the lock is the explicit guard
// that turns daemon shutdown into a migration prerequisite.
func AcquireDatabaseRuntimeLock(databasePath string) (func(), error) {
	lockPath := databasePath + ".runtime.lock"
	file, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open database runtime lock: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure database runtime lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrDatabaseInUse
		}
		return nil, fmt.Errorf("acquire database runtime lock: %w", err)
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}, nil
}
