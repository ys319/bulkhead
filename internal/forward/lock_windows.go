package forward

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// The lock is on one byte far past anything the file holds: a locked range
// cannot be read by other processes there, and the note at the start of
// the file is for them.
const lockOffset = 1 << 30

func lockRange() *windows.Overlapped { return &windows.Overlapped{Offset: lockOffset} }

func lockFile(file *os.File) error {
	err := windows.LockFileEx(windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, lockRange())
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return ErrHeld
	}
	return err
}

func unlockFile(file *os.File) error {
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, lockRange())
}
