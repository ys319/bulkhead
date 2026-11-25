package forward

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// A named set is opened by one bh at a time: two would fight over the
// ports, and neither could tell that from a port some other program has.
// Who has it is a lock of the operating system on a file — it goes when
// the process does, however that ends, so nothing is ever left locked by
// a bh that is no longer there.

// ErrHeld means another process has the set open.
var ErrHeld = errors.New("another bh has these forwards open")

// Lock is the hold on a set.
type Lock struct{ file *os.File }

// Acquire takes the lock at path without waiting; ErrHeld when another
// process has it. The file is made if it is missing, and says who holds it.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := lockFile(file); err != nil {
		file.Close()
		return nil, err
	}
	// For whoever finds it held: which process to look for.
	if err := file.Truncate(0); err == nil {
		_, _ = file.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &Lock{file: file}, nil
}

// Release gives the lock up.
func (l *Lock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = l.file.Truncate(0)
	_ = unlockFile(l.file)
	_ = l.file.Close()
	l.file = nil
}

// Holder is the process that has the lock at path, 0 when nobody has (or
// when the one that has did not say). It asks by trying: the lock is the
// truth, the file's contents only a note.
func Holder(path string) (pid int, held bool) {
	lock, err := Acquire(path)
	if err == nil {
		lock.Release()
		return 0, false
	}
	if !errors.Is(err, ErrHeld) {
		return 0, false
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, true
	}
	defer file.Close()
	note, _ := io.ReadAll(io.LimitReader(file, 32))
	pid, _ = strconv.Atoi(strings.TrimSpace(string(note)))
	return pid, true
}

// HeldBy words who has the lock.
func HeldBy(pid int) string {
	if pid > 0 {
		return fmt.Sprintf("another bh (process %d)", pid)
	}
	return "another bh"
}
