package main

import (
	"os"

	"golang.org/x/term"
)

// rawTerminal puts the terminal in raw mode, for a shell that runs in a
// container: every key goes there, and what comes back is shown as it is.
func rawTerminal() (restore func(), size func() (height, width uint), err error) {
	input := int(os.Stdin.Fd())
	state, err := term.MakeRaw(input)
	if err != nil {
		return nil, nil, err
	}
	undoOutput := enableTerminalOutput()
	restore = func() {
		undoOutput()
		_ = term.Restore(input, state)
	}
	size = func() (uint, uint) {
		width, height, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil || width <= 0 || height <= 0 {
			return 0, 0
		}
		return uint(height), uint(width)
	}
	return restore, size, nil
}
