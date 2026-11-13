package main

import (
	"slices"
	"strings"
	"testing"
)

// Every command says how it is used when asked — on standard output, with
// status 0, without doing anything — and says the same to bh help.
func TestEveryCommandAnswersHelp(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	calls := len(b.engine.Calls)

	for _, name := range completedNames() {
		if name == "help" {
			continue
		}
		asked := b.ok("help", name)
		if !strings.HasPrefix(asked, "usage:") || !strings.Contains(asked, "bh "+name) {
			t.Errorf("bh help %s:\n%s", name, asked)
		}
		for _, flag := range []string{"--help", "-h"} {
			code, stdout, stderr := b.run(name, flag)
			if code != 0 || stdout != asked || stderr != "" {
				t.Errorf("bh %s %s: exit %d, stderr %q\n%s", name, flag, code, stderr, stdout)
			}
		}
		// After the environment, too.
		if code, stdout, stderr := b.run(name, "web", "--help"); name != "ssh" && (code != 0 || stdout != asked || stderr != "") {
			t.Errorf("bh %s web --help: exit %d, stderr %q\n%s", name, code, stderr, stdout)
		}
	}
	// A subcommand answers with its command's text.
	for _, command := range completedCommands {
		for _, sub := range command.subs {
			code, stdout, stderr := b.run(command.name, sub.name, "--help")
			if code != 0 || stdout != b.ok("help", command.name) || stderr != "" {
				t.Errorf("bh %s %s --help: exit %d, stderr %q\n%s", command.name, sub.name, code, stderr, stdout)
			}
		}
	}
	if len(b.engine.Calls) != calls {
		t.Errorf("asking for help did something: %v", b.engine.Calls[calls:])
	}
	if got := b.env("web"); got.Name != "web" {
		t.Errorf("web became %+v", got)
	}
}

// One command's help is its own lines of the list, and no other's.
func TestHelpForOneCommandIsItsOwnPart(t *testing.T) {
	b := newBench(t)
	restart := b.ok("help", "restart")
	for _, want := range []string{"bh restart <env>", "--rebuild", "builds the image again first"} {
		if !strings.Contains(restart, want) {
			t.Errorf("bh help restart lacks %q:\n%s", want, restart)
		}
	}
	if strings.Contains(restart, "all environments") || strings.Contains(restart, "down") {
		t.Errorf("bh help restart has another command's lines:\n%s", restart)
	}
	// The last entry stops before the closing paragraph.
	if completion := b.ok("help", "completion"); strings.Contains(completion, "Commands ask for nothing") {
		t.Errorf("bh help completion runs on:\n%s", completion)
	}
	// The commands with subcommands answer with their own text.
	if token := b.ok("help", "token"); token != tokenUsage+"\n" {
		t.Errorf("bh help token:\n%s", token)
	}
	// Without a command, or with --help in its place: the list.
	for _, line := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		if got := b.ok(line...); got != usageText {
			t.Errorf("bh %v:\n%s", line, got)
		}
	}
	if stderr := b.fails(2, "help", "nosuch"); !strings.Contains(stderr, `unknown command "nosuch"`) {
		t.Errorf("bh help nosuch: %s", stderr)
	}
}

// --help among the flags, further along the line, is still a request for
// help: nothing is done.
func TestHelpAmongTheFlagsDoesNothing(t *testing.T) {
	b := newBench(t)
	if out := b.ok("create", "web", "--port", "23001", "--help"); out != b.ok("help", "create") {
		t.Errorf("bh create web --port 23001 --help:\n%s", out)
	}
	cfg, err := b.store().Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Envs) != 0 {
		t.Errorf("an environment was made: %v", cfg.Envs)
	}
}

// What follows the environment of bh ssh is a command for the other side:
// its --help is not bh's.
func TestHelpIsNotTakenFromACommandForTheOtherSide(t *testing.T) {
	cases := []struct {
		command string
		args    []string
		want    bool
	}{
		{"up", []string{"--help"}, true},
		{"up", []string{"web", "-h"}, true},
		{"token", []string{"set", "--help"}, true},
		{"token", []string{"set", "web", "--help"}, false}, // the flags see it
		{"ssh", []string{"--help"}, true},
		{"ssh", []string{"web", "--help"}, false},
		{"ssh", []string{"web", "ls", "--help"}, false},
		{"var", []string{"--", "--help"}, false},
		{"up", nil, false},
	}
	for _, c := range cases {
		if got := helpWanted(c.command, c.args); got != c.want {
			t.Errorf("helpWanted(%q, %v) = %v", c.command, c.args, got)
		}
	}
}

// bh help <Tab> offers the commands.
func TestCompletionOffersTheCommandsToHelp(t *testing.T) {
	b := newBench(t)
	got := b.completes("help", "")
	if !slices.Contains(got, "token") || !slices.Contains(got, "up") || slices.Contains(got, "help") {
		t.Errorf("bh help <Tab>: %v", got)
	}
}
