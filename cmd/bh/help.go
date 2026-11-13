package main

import (
	"errors"
	"regexp"
	"strings"
)

// Help is the same text wherever it is asked for: `bh help` and
// `bh --help` print the list of commands; `bh help <command>` and
// `bh <command> --help` print that command's part — its own usage text
// where it has one (the commands with subcommands, and configure), its
// entry in the list otherwise. It goes to standard output with status 0:
// asking how a command is used is not an error.

// errHelpAsked is what a command returns when --help stood among its
// flags, past where helpWanted looks.
var errHelpAsked = errors.New("help asked for")

// ownUsage are the commands that have a usage text of their own.
var ownUsage = map[string]string{
	"configure": configureUsage,
	"key":       keyUsage,
	"remote":    remoteUsage,
	"var":       varUsage,
	"secret":    secretUsage,
	"token":     tokenUsage,
	"snapshot":  snapshotUsage,
}

// An entry of the list starts two spaces in, with the command's name;
// what follows further in belongs to it.
var usageEntry = regexp.MustCompile(`^  ([a-z][a-z-]*)( |$)`)

// commandHelp is the help for one command; false when the list of commands
// has no such entry.
func commandHelp(name string) (string, bool) {
	if text, has := ownUsage[name]; has {
		return text + "\n", true
	}
	var block []string
	for _, line := range strings.Split(usageText, "\n") {
		if match := usageEntry.FindStringSubmatch(line); match != nil {
			if len(block) > 0 {
				break // the next command's entry
			}
			if match[1] == name {
				// "  up <env>   start..." reads as "bh up <env>   start...".
				block = append(block, "bh "+strings.TrimPrefix(line, "  "))
			}
			continue
		}
		if len(block) > 0 {
			if strings.TrimSpace(line) == "" {
				break
			}
			block = append(block, "   "+line)
		}
	}
	if len(block) == 0 {
		return "", false
	}
	return "usage:\n  " + strings.Join(block, "\n  ") + "\n", true
}

// helpWanted reports whether the arguments of a command ask for its help:
// --help or -h as the first or second word (bh token --help, bh token set
// --help). Further on it is not bh's to take — and for ssh not even
// second: what follows the environment there is a command for the other
// side.
func helpWanted(command string, args []string) bool {
	reach := 2
	if command == "ssh" {
		reach = 1
	}
	for i, arg := range args {
		if i >= reach || arg == "--" {
			break
		}
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}
