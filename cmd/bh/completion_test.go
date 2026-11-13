package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ys319/bulkhead/internal/config"
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

// The descriptions travel as "name<TAB>description" lines and stand inside
// zsh's 'name:description': a colon, a tab or a line break in one would be
// read as syntax.
func TestCompletionDescriptionsAreOneField(t *testing.T) {
	check := func(where, about string) {
		t.Helper()
		if strings.ContainsAny(about, ":\t\n") {
			t.Errorf("%s: %q has a character the scripts take as syntax", where, about)
		}
	}
	for _, command := range completedCommands {
		if command.about == "" {
			t.Errorf("%s has no description", command.name)
		}
		check(command.name, command.about)
		for _, sub := range command.subs {
			if sub.about == "" {
				t.Errorf("%s %s has no description", command.name, sub.name)
			}
			check(command.name+" "+sub.name, sub.about)
		}
	}
	for _, service := range tokenServices {
		check(service.Name, service.Title)
		for _, preset := range service.Presets {
			check(service.Name+" "+preset.Name, preset.About)
		}
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
	// A subcommand of the table is one bh knows: it stops at its own
	// usage, not at the command's.
	for _, command := range completedCommands {
		for _, sub := range command.subs {
			if _, _, stderr := b.run(command.name, sub.name, "--no-such-flag"); strings.Contains(stderr, "usage: bh "+command.name+" <subcommand>") {
				t.Errorf("the completion offers %q %q, which bh does not know", command.name, sub.name)
			}
		}
	}
}

// offered asks bh what it offers for the last word of the line and returns
// the names, in order.
func (b *bench) completes(line ...string) []string {
	b.t.Helper()
	code, stdout, stderr := b.run(append([]string{"__complete", "--"}, line...)...)
	if code != 0 || stderr != "" {
		b.t.Fatalf("__complete %q: exit %d, stderr %q", line, code, stderr)
	}
	var names []string
	for _, entry := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
		if entry != "" {
			name, _, _ := strings.Cut(entry, "\t")
			names = append(names, name)
		}
	}
	return names
}

// Choosing is the completion's part: for every position bh says what goes
// there — from its table, and from what the environments have.
func TestCompletionOffersWhatGoesAtEachPosition(t *testing.T) {
	b := newBench(t)
	withData(t, b, "web")
	b.ok("create", "api")
	b.ok("remote", "add", "example.test:22/box")
	b.ok("create", "old")
	old := b.env("old")
	old.Archived = "2026-11-01T000000Z.tar.gz"
	if err := (config.Store{Dir: b.dir}).SaveEnvironment(old); err != nil {
		t.Fatal(err)
	}
	b.ok("var", "set", "web", "NODE_ENV", "development")
	b.ok("var", "set", "web", "NAME", "x")
	b.stdin = "v"
	b.ok("secret", "set", "web", "API_KEY")
	b.seedGitHub("web", patNew)
	b.ok("snapshot", "create", "web", "--note", "first")
	b.tick()
	b.ok("snapshot", "create", "web")
	line, _ := publicKey(t, "laptop")
	b.stdin = line
	b.ok("key", "add", "--label", "laptop")
	line, _ = publicKey(t, "phone")
	b.stdin = line
	b.ok("key", "add", "--label", "phone")
	b.ok("key", "attach", "web", "phone")
	b.engine.Calls = nil
	kept := len(b.keychain)

	for _, test := range []struct {
		line []string
		want []string
	}{
		// Commands and their parts.
		{[]string{"to"}, []string{"token"}},
		{[]string{"-"}, []string{"--version", "--help"}},
		{[]string{"token", ""}, []string{"list", "set", "refresh", "remove"}},
		{[]string{"token", "re"}, []string{"refresh", "remove"}},
		{[]string{"completion", ""}, []string{"zsh", "bash"}},
		{[]string{"secret", "store", ""}, []string{"keychain", "file"}},
		{[]string{"nosuch", ""}, nil},
		{[]string{"token", "nosuch", ""}, nil},

		// Environments, by what the command can be used on.
		{[]string{"status", ""}, []string{"api", "box", "old", "web"}},
		{[]string{"up", ""}, []string{"api", "web"}},
		{[]string{"up", "w"}, []string{"web"}},
		{[]string{"unarchive", ""}, []string{"old"}},
		{[]string{"remote", "remove", ""}, []string{"box"}},
		{[]string{"create", "--from", ""}, []string{"api", "old", "web"}},
		{[]string{"up", "web", ""}, nil},

		// Tokens: the service, then the presets of that service.
		{[]string{"token", "set", ""}, []string{"api", "web"}},
		{[]string{"token", "set", "web", ""}, []string{"github", "huggingface"}},
		{[]string{"token", "set", "web", "github", ""}, []string{"read", "push", "contribute"}},
		{[]string{"token", "set", "web", "github", "p"}, []string{"push"}},
		{[]string{"token", "set", "web", "huggingface", ""}, nil},
		{[]string{"token", "set", "web", "hf", ""}, nil},
		// What the environment has.
		{[]string{"token", "refresh", "web", ""}, []string{"github"}},
		{[]string{"token", "remove", "api", ""}, nil},
		{[]string{"var", "unset", "web", ""}, []string{"NAME", "NODE_ENV"}},
		{[]string{"var", "unset", "web", "NO"}, []string{"NODE_ENV"}},
		{[]string{"var", "set", "web", "NODE_ENV", ""}, nil},
		{[]string{"secret", "remove", "web", ""}, []string{"API_KEY"}},
		{[]string{"rollback", "web", ""}, []string{"latest", "2026-11-13T090100Z", "2026-11-13T090000Z"}},
		{[]string{"snapshot", "remove", "web", ""}, []string{"2026-11-13T090100Z", "2026-11-13T090000Z"}},
		{[]string{"rollback", "api", ""}, nil},
		{[]string{"key", "attach", "web", ""}, []string{"laptop", "phone"}},
		{[]string{"key", "detach", "web", ""}, []string{"phone"}},
		{[]string{"key", "remove", ""}, []string{"laptop", "phone"}},

		// Flags: those of the command, each once; and what follows one.
		{[]string{"token", "set", "web", "github", "--"}, []string{"--owner", "--days", "--no-open"}},
		{[]string{"token", "set", "--no-open", "web", "--"}, []string{"--owner", "--days"}},
		{[]string{"token", "set", "--owner", ""}, nil},
		{[]string{"token", "set", "--owner", "acme", "web", ""}, []string{"github", "huggingface"}},
		{[]string{"token", "set", "--owner=acme", "web", ""}, []string{"github", "huggingface"}},
		{[]string{"rollback", "web", "--only", ""}, []string{"workspace", "home"}},
		{[]string{"rollback", "--only", "home", "web", ""}, []string{"latest", "2026-11-13T090100Z", "2026-11-13T090000Z"}},
		{[]string{"configure", "web", "--gpu", ""}, []string{"off", "legacy", "cdi"}},
		{[]string{"configure", "web", "--publish", ""}, []string{"on", "off"}},
		{[]string{"create", "new", "--restart", "u"}, []string{"unless-stopped"}},
		{[]string{"reset", "web", "--"}, []string{"--home", "--no-snapshot", "--yes"}},

		// Where a file's name goes, the shell completes.
		{[]string{"export", "web", ""}, []string{"__files__"}},
		{[]string{"import", ""}, []string{"__files__"}},
		{[]string{"import", "some.tar.gz", ""}, nil},
	} {
		if got := b.completes(test.line...); !slices.Equal(got, test.want) {
			t.Errorf("bh %s⇥\n  got  %q\n  want %q", strings.Join(test.line, " "), got, test.want)
		}
	}

	// At the very start: every command, with what it is for.
	all := b.completes("")
	if !slices.Equal(all, completedNames()) {
		t.Errorf("at the start: %q", all)
	}
	_, described, _ := b.run("__complete", "--", "token", "set", "web", "github", "")
	if !strings.Contains(described, "push\talso push") {
		t.Errorf("the presets come without what they allow:\n%s", described)
	}
	_, held, _ := b.run("__complete", "--", "token", "refresh", "web", "")
	if !strings.Contains(held, "github\tpush · octo") || strings.Contains(held, "github_pat") {
		t.Errorf("the tokens it has:\n%s", held)
	}

	// It answers while the user types: from the directory alone.
	if len(b.engine.Calls) != 0 {
		t.Errorf("a completion asked Docker: %v", b.engine.Calls)
	}
	if len(b.keychain) != kept {
		t.Errorf("a completion changed the keychain: %v", b.keychain)
	}
}

// On any problem it says nothing: an error would be printed into the
// command line being typed.
func TestCompletionIsSilentWhenItCannotAnswer(t *testing.T) {
	b := newBench(t)
	// Nothing there yet.
	for _, line := range [][]string{{"up", ""}, {"token", "refresh", "web", ""}, {"rollback", "web", ""}, nil} {
		if code, stdout, stderr := b.run(append([]string{"__complete", "--"}, line...)...); code != 0 || stdout != "" || stderr != "" {
			t.Errorf("no configuration, %q: exit %d, %q, %q", line, code, stdout, stderr)
		}
	}
	if _, err := os.Stat(b.dir); !os.IsNotExist(err) {
		t.Errorf("a completion made the configuration directory: %v", err)
	}

	// A directory bh cannot load (two environments with one name).
	b.ok("create", "web")
	b.ok("create", "api")
	web := b.env("web")
	data, err := os.ReadFile(filepath.Join(b.dir, "environments", b.env("api").ID, "env.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.dir, "environments", web.ID, "env.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	b.fails(1, "list")
	for _, line := range [][]string{{"up", ""}, {"var", "unset", "web", ""}} {
		if code, stdout, stderr := b.run(append([]string{"__complete", "--"}, line...)...); code != 0 || stdout != "" || stderr != "" {
			t.Errorf("unloadable configuration, %q: exit %d, %q, %q", line, code, stdout, stderr)
		}
	}
	// The commands themselves need no configuration.
	if got := b.completes("to"); !slices.Equal(got, []string{"token"}) {
		t.Errorf("commands with an unloadable configuration: %q", got)
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

// The bash script, run: it hands bh the words up to the cursor — an empty
// one included — and offers the names bh answers with, descriptions cut.
func TestBashScriptCarriesTheLineAndTheAnswer(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	dir := t.TempDir()
	// A bh that records what it was asked and answers two candidates.
	fake := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + filepath.Join(dir, "asked") + "\"\nprintf 'push\\talso push and more\\ncontribute\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "bh"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	script, _ := completionScript("bash")
	// A bash that is not built for a terminal has no completion builtins;
	// the script only needs them to register itself.
	script = "complete() { :; }\ncompopt() { :; }\n" + script
	run := exec.Command(bash, "-c", script+"\nCOMP_WORDS=(bh token set web github '' later); COMP_CWORD=5; _bh; printf '%s\\n' \"${COMPREPLY[@]}\"")
	run.Env = []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")}
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if string(out) != "push\ncontribute\n" {
		t.Errorf("offered:\n%s", out)
	}
	asked, err := os.ReadFile(filepath.Join(dir, "asked"))
	if err != nil || string(asked) != "__complete\n--\ntoken\nset\nweb\ngithub\n\n" {
		t.Errorf("bh was asked %q (%v)", asked, err)
	}
}
