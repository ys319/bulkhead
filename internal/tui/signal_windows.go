package tui

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyTermination reports the signals that ask the program to end. On
// Windows closing the console arrives as SIGTERM.
func notifyTermination(to chan<- os.Signal) { signal.Notify(to, syscall.SIGTERM) }
