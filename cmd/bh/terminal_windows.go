package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableTerminalOutput makes the console interpret the escape sequences a
// remote shell sends. Windows Terminal does by default; the classic
// console host has to be told.
func enableTerminalOutput() (undo func()) {
	handle := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return func() {}
	}
	if err := windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return func() {}
	}
	return func() { _ = windows.SetConsoleMode(handle, mode) }
}
