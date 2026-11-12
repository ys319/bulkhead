package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func completedNames() []string {
	names := make([]string, len(completedCommands))
	for i, command := range completedCommands {
		names[i] = command.name
	}
	return names
}

func TestCompletionPrintsAScript(t *testing.T) {
	b := newBench(t)
	if out := b.ok("completion", "zsh"); !strings.HasSuffix(out, "\ncompdef _bh bh\n") {
		t.Errorf("zsh: the script does not end by registering itself:\n%s", out)
	}
	if out := b.ok("completion", "bash"); !strings.HasSuffix(out, "\ncomplete -F _bh bh\n") {
		t.Errorf("bash: the script does not end by registering itself:\n%s", out)
	}
	if stderr := b.fails(2, "completion"); !strings.Contains(stderr, "usage: bh completion zsh|bash") {
		t.Errorf("stderr: %s", stderr)
	}
	if stderr := b.fails(2, "completion", "fish"); !strings.Contains(stderr, `"fish" is not a supported shell`) {
		t.Errorf("stderr: %s", stderr)
	}
	// It is eval'ed from an rc file: nothing may be written, anywhere.
	if _, err := os.Stat(b.dir); !os.IsNotExist(err) {
		t.Errorf("completion made the configuration directory: %v", err)
	}
}

func TestCompletionScriptsCarryTheWholeVocabulary(t *testing.T) {
	for _, shell := range []string{"zsh", "bash"} {
		script, err := completionScript(shell)
		if err != nil {
			t.Fatal(err)
		}
		for _, command := range completedCommands {
			if !regexp.MustCompile(`(^|[\s'"|])` + regexp.QuoteMeta(command.name) + `($|[\s'":|)])`).MatchString(script) {
				t.Errorf("%s: the command %s is missing", shell, command.name)
			}
			for _, flag := range command.flags {
				if !strings.Contains(script, flag) {
					t.Errorf("%s: the flag %s of %s is missing", shell, flag, command.name)
				}
			}
			for _, sub := range command.subs {
				if !strings.Contains(script, sub.name) {
					t.Errorf("%s: %s %s is missing", shell, command.name, sub.name)
				}
			}
		}
		// A slip in putting the script together shows as a formatting verb
		// left behind.
		if strings.Contains(script, "%!") || strings.Contains(script, "%s") {
			t.Errorf("%s: the script has an unfilled place:\n%s", shell, script)
		}
	}

	zsh, _ := completionScript("zsh")
	for _, command := range completedCommands {
		if !strings.Contains(zsh, "'"+command.name+":"+command.about+"'") {
			t.Errorf("zsh: no description for %s", command.name)
		}
	}
	for _, part := range []string{
		"'attach[let a key log in to an environment]'",
		"'configure[change host or port]'",
		"compadd -- --version --help",
		"_values 'completion command' zsh bash",
	} {
		if !strings.Contains(zsh, part) {
			t.Errorf("zsh lacks %q", part)
		}
	}
	bash, _ := completionScript("bash")
	for _, part := range []string{
		`compgen -W "--version --help" -- "$cur"`,
		`compgen -W "` + strings.Join(completedNames(), " ") + `" -- "$cur"`,
		`compgen -W "zsh bash" -- "$cur"`,
	} {
		if !strings.Contains(bash, part) {
			t.Errorf("bash lacks %q", part)
		}
	}
}

// The descriptions stand unquoted inside zsh's 'name:description' and
// 'name[description]': one apostrophe ("machine's") and the script is an
// unmatched quote.
func TestCompletionDescriptionsFitZshsForms(t *testing.T) {
	check := func(where, about string) {
		if strings.ContainsAny(about, `:'[]\`) {
			t.Errorf("%s: %q has a character zsh takes as syntax", where, about)
		}
	}
	for _, command := range completedCommands {
		if command.about == "" {
			t.Errorf("%s has no description", command.name)
		}
		check(command.name, command.about)
		for _, sub := range command.subs {
			check(command.name+" "+sub.name, sub.about)
		}
	}
	for _, kind := range tokenKinds {
		check("token kind "+kind.name, kind.about)
	}
}

// The commands are the map in main.go, the completion has its own table.
// The two are held against each other from both sides: through bh itself
// (does it know the command?) and through the usage text.
func TestCompletionKnowsTheCommands(t *testing.T) {
	elsewhere := []string{"help"}

	b := newBench(t)
	for _, name := range completedNames() {
		if slices.Contains(elsewhere, name) {
			continue
		}
		// An argument no command takes: each stops at its own usage, or at
		// "no such environment", before it does anything.
		if _, _, stderr := b.run(name, "--no-such-flag"); strings.Contains(stderr, "unknown command") {
			t.Errorf("the completion offers %q, which bh does not know", name)
		}
	}
	if len(b.engine.Calls) != 0 {
		t.Fatalf("a command did something: %v", b.engine.Calls)
	}

	listed := regexp.MustCompile(`(?m)^  ([a-z][a-z-]*)`).FindAllStringSubmatch(usageText, -1)
	if len(listed) < 10 {
		t.Fatalf("the usage text was not understood: %v", listed)
	}
	for _, match := range listed {
		if !slices.Contains(completedNames(), match[1]) {
			t.Errorf("bh %s is in the usage text and not in the completion", match[1])
		}
	}
	// The helper the scripts call is theirs alone.
	if slices.Contains(completedNames(), "__complete") || strings.Contains(usageText, "__complete") {
		t.Error("__complete is offered to the user")
	}
}

// The names of the environments come from bh itself while the user types:
// from the configuration directory alone, and without a word on any
// problem.
func TestCompletionHelperPrintsTheEnvironments(t *testing.T) {
	b := newBench(t)
	// Nothing there yet: nothing to offer, nothing to say.
	if code, stdout, stderr := b.run("__complete", "envs"); code != 0 || stdout != "" || stderr != "" {
		t.Errorf("no configuration: exit %d, %q, %q", code, stdout, stderr)
	}

	b.ok("create", "web")
	b.ok("create", "api")
	b.ok("remote", "add", "example.test:22/box")
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"envs"}, "api\nbox\nweb\n"},
		{[]string{"envs", "all"}, "api\nbox\nweb\n"},
		{[]string{"envs", "local"}, "api\nweb\n"},
		{[]string{"envs", "remote"}, "box\n"},
		{[]string{"keys"}, ""},
		{nil, ""},
	} {
		if got := b.ok(append([]string{"__complete"}, tt.args...)...); got != tt.want {
			t.Errorf("__complete %v = %q, want %q", tt.args, got, tt.want)
		}
	}
	if len(b.engine.Calls) != 0 {
		t.Errorf("a completion asked Docker: %v", b.engine.Calls)
	}

	// A directory bh cannot load (two environments with one name): an
	// error here would be printed into the command line being typed.
	web := b.env("web")
	web.Name = "api"
	data, err := os.ReadFile(filepath.Join(b.dir, "environments", b.env("api").ID, "env.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.dir, "environments", web.ID, "env.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	b.fails(1, "list")
	if code, stdout, stderr := b.run("__complete", "envs"); code != 0 || stdout != "" || stderr != "" {
		t.Errorf("unloadable configuration: exit %d, %q, %q", code, stdout, stderr)
	}
}

// The shells themselves read the scripts, where they are installed: a
// quoting slip is a syntax error for every user of that shell.
func TestCompletionScriptsParse(t *testing.T) {
	for _, shell := range []string{"zsh", "bash"} {
		t.Run(shell, func(t *testing.T) {
			program, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s is not installed", shell)
			}
			script, err := completionScript(shell)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "bh-completion")
			if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
				t.Fatal(err)
			}
			// -n: read the commands and check them, run nothing.
			if out, err := exec.Command(program, "-n", path).CombinedOutput(); err != nil {
				t.Errorf("%s does not accept the script: %v\n%s", shell, err, out)
			}
		})
	}
}
