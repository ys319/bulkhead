//go:build !windows

package main

// enableTerminalOutput has nothing to do where terminals interpret escape
// sequences as a matter of course.
func enableTerminalOutput() (undo func()) { return func() {} }
