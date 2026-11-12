//go:build !windows

package tui

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyTermination reports the signals that ask the program to end: a kill
// and the terminal going away.
func notifyTermination(to chan<- os.Signal) { signal.Notify(to, syscall.SIGTERM, syscall.SIGHUP) }
