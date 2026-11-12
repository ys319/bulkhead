package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

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

// readHiddenLine prompts and reads one line from the terminal without
// showing it: a pasted secret.
//
// Raw mode rather than just "no echo": in line mode the terminal driver
// holds the line and drops what does not fit (1024 bytes on macOS) — a long
// token would be saved short without a word.
func readHiddenLine(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	input := int(os.Stdin.Fd())
	state, err := term.MakeRaw(input)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = term.Restore(input, state)
		fmt.Fprintln(os.Stderr)
	}()
	return scanHiddenLine(os.Stdin)
}

// The marks a terminal puts around pasted text when a program asked for
// them. bh did not, but a shell that did may not have taken it back.
var pasteMarks = strings.NewReplacer("\x1b[200~", "", "\x1b[201~", "")

// scanHiddenLine reads the keys of a raw terminal up to the end of the
// line. In raw mode the keys that normally act arrive as plain bytes.
func scanHiddenLine(in io.Reader) (string, error) {
	var line []byte
	buffer := make([]byte, 4096)
	for {
		n, err := in.Read(buffer)
		for _, key := range buffer[:n] {
			switch {
			case key == 3: // Ctrl-C
				return "", errors.New("interrupted — nothing was changed")
			case key == '\r' || key == '\n':
				return pasteMarks.Replace(string(line)), nil
			case key == 4 && len(line) == 0: // Ctrl-D
				return "", nil
			case key == 127 || key == 8: // backspace
				if len(line) > 0 {
					line = line[:len(line)-1]
				}
			default:
				line = append(line, key)
			}
		}
		if errors.Is(err, io.EOF) {
			return pasteMarks.Replace(string(line)), nil
		}
		if err != nil {
			return "", err
		}
	}
}
