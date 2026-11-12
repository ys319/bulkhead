package main

import (
	"errors"
	"maps"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ys319/bulkhead/internal/config"
)

// tokens are the environment's recorded tokens as NAME=value, in order.
func (b *bench) tokens(name string) []string {
	b.t.Helper()
	entries, _, err := b.store().LoadTokens(b.env(name).ID)
	if err != nil {
		b.t.Fatal(err)
	}
	list := []string{}
	for _, entry := range entries {
		list = append(list, entry.Name+"="+entry.Value)
	}
	return list
}

func TestTokenSetListRemove(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	id := b.env("web").ID
	if out := b.ok("token", "list", "web"); !strings.Contains(out, `"web" has no tokens (bh token set web hf | custom <NAME>; GitHub: bh github set)`) {
		t.Errorf("list:\n%s", out)
	}

	// Before the first start: the volume is made for it.
	b.stdin = "v1\n"
	b.now = time.Date(2026, 10, 12, 6, 30, 0, 0, time.UTC)
	code, out, stderr := b.run("token", "set", "web", "custom", "K")
	if code != 0 || out != "Set K for \"web\" — new shells see it (running shells keep the old value)\n" || stderr != "" {
		t.Fatalf("exit %d\n%s%s", code, out, stderr)
	}
	if calls := b.engine.Calls; len(calls) != 2 || calls[0] != "volume bulkhead-"+id+"_tokens" || calls[1] != "write-volume bulkhead-"+id+"_tokens" {
		t.Errorf("calls = %v", calls)
	}
	run := b.lastRun()
	if string(run.Input) != "K=v1\n" || run.Job.Target != "/t" || !strings.HasPrefix(run.Job.Image, "bulkhead-holder:") || strings.Contains(run.Job.Script, "v1") {
		t.Errorf("run = %+v", run)
	}
	const record = `{
  "tokens": [
    {
      "kind": "custom",
      "name": "K",
      "value": "v1",
      "addedAt": "2026-10-12T06:30:00.000Z"
    }
  ]
}
`
	if data, _ := os.ReadFile(b.store().TokensFile(id)); string(data) != record {
		t.Errorf("tokens.json:\n%s", data)
	}
	if info, err := os.Stat(b.store().TokensFile(id)); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Errorf("tokens.json: %v, %v", info.Mode(), err)
	}

	// Hugging Face: the page is opened, the name is fixed.
	b.typing, b.secret = true, "hf_secretvalue\n"
	code, out, stderr = b.run("token", "set", "web", "hf")
	if code != 0 || !strings.Contains(out, "https://huggingface.co/settings/tokens") || !strings.Contains(out, `Set HF_TOKEN for "web"`) ||
		!strings.Contains(stderr, "Paste the token (input is hidden): ") {
		t.Fatalf("exit %d\n%s%s", code, out, stderr)
	}
	nowhere(t, "hf_secretvalue", out, stderr)
	if len(b.opened) != 1 || b.opened[0] != "https://huggingface.co/settings/tokens" {
		t.Errorf("opened %v", b.opened)
	}
	b.ok("token", "set", "web", "hf", "--no-open")
	if len(b.opened) != 1 {
		t.Errorf("--no-open opened %v", b.opened)
	}
	if string(b.lastRun().Input) != "K=v1\nHF_TOKEN=hf_secretvalue\n" {
		t.Errorf("input = %q", b.lastRun().Input)
	}

	// A token of the same name is replaced, where it stands.
	b.typing, b.stdin = false, "v2"
	if out := b.ok("token", "set", "web", "custom", "K"); !strings.HasPrefix(out, `Replaced K for "web" — new shells see it`) {
		t.Errorf("output:\n%s", out)
	}
	if got := strings.Join(b.tokens("web"), " "); got != "K=v2 HF_TOKEN=hf_secretvalue" {
		t.Errorf("tokens = %s", got)
	}

	// The list names GitHub's too, and never a value.
	b.seedGitHub("web", patNew)
	b.now = time.Date(2026, 10, 30, 6, 30, 0, 0, time.UTC)
	out = b.ok("token", "list", "web")
	want := "GH_TOKEN        github · push · octo · 2 repos · expires in 12d\n" +
		"K               custom\n" +
		"HF_TOKEN        hf\n"
	if out != want {
		t.Errorf("list:\n%s\nwant:\n%s", out, want)
	}

	volumes := maps.Clone(b.engine.Volumes)
	b.engine.Calls = nil
	out = b.ok("token", "remove", "web", "HF_TOKEN")
	if out != "Removed HF_TOKEN from \"web\" — new shells no longer see it (running shells keep it; it is not revoked at its service — https://huggingface.co/settings/tokens)\n" {
		t.Errorf("output:\n%s", out)
	}
	if string(b.lastRun().Input) != "K=v2\n" {
		t.Errorf("input = %q", b.lastRun().Input)
	}
	// The last one gone: the record stays, as an empty list, and the file
	// in the volume is written empty. The volume is not removed.
	out = b.ok("token", "remove", "web", "K")
	if strings.Contains(out, "huggingface") {
		t.Errorf("output:\n%s", out)
	}
	if data, _ := os.ReadFile(b.store().TokensFile(id)); string(data) != "{\n  \"tokens\": []\n}\n" {
		t.Errorf("tokens.json = %q", data)
	}
	if run := b.lastRun(); len(run.Input) != 0 || run.Job.Volume != "bulkhead-"+id+"_tokens" {
		t.Errorf("run = %+v", run)
	}
	b.noVolumeRemoved(volumes)

	if stderr := b.fails(1, "token", "remove", "web", "K"); !strings.Contains(stderr, `"web" has no token named K`) {
		t.Errorf("stderr: %s", stderr)
	}
	if stderr := b.fails(1, "token", "remove", "web", "GH_TOKEN"); !strings.Contains(stderr, "bh github revoke web") {
		t.Errorf("stderr: %s", stderr)
	}
	if b.githubState("web") == nil {
		t.Error("the GitHub token is gone")
	}
}

func TestTokenSetRefuses(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.ok("remote", "add", "studio.test:22001/main")
	// Typing, so that asking for the token shows: a name that is turned
	// down is turned down before anything is pasted.
	b.typing = true
	tests := []struct {
		name   string
		code   int
		secret string
		args   []string
		want   string
	}{
		{"a reserved name", 1, "v", []string{"set", "web", "custom", "HOME"}, "reserved"},
		{"a name that is no variable", 1, "v", []string{"set", "web", "custom", "lower"}, "A-Z"},
		{"the GitHub token's name", 1, "v", []string{"set", "web", "custom", "GH_TOKEN"}, "bh github set"},
		{"the Hugging Face token as custom", 1, "v", []string{"set", "web", "custom", "HF_TOKEN"}, "Hugging Face"},
		{"a remote environment", 1, "v", []string{"set", "main", "custom", "K"}, "remote environment"},
		{"hf with a name", 2, "v", []string{"set", "web", "hf", "X"}, "usage: bh token set"},
		{"custom without a name", 2, "v", []string{"set", "web", "custom"}, "usage: bh token set"},
		{"an unknown kind", 2, "v", []string{"set", "web", "github", "X"}, "usage: bh token set"},
		{"no subcommand", 2, "v", nil, "usage: bh token <subcommand>"},
		{"an unknown subcommand", 2, "v", []string{"show", "web"}, "usage: bh token <subcommand>"},
		{"list without an environment", 2, "v", []string{"list"}, "usage: bh token list"},
		{"remove without a name", 2, "v", []string{"remove", "web"}, "usage: bh token remove"},
		{"list of a remote environment", 1, "v", []string{"list", "main"}, "remote environment"},
		{"remove from a remote environment", 1, "v", []string{"remove", "main", "K"}, "remote environment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b.secret = tt.secret
			stderr := b.fails(tt.code, append([]string{"token"}, tt.args...)...)
			if !strings.Contains(stderr, tt.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr, tt.want)
			}
			if strings.Contains(stderr, "Paste the token") {
				t.Errorf("the token was asked for:\n%s", stderr)
			}
		})
	}

	for _, value := range []string{"a b", "a'b", "", "line\nline", "日本"} {
		b.secret = value
		if stderr := b.fails(1, "token", "set", "web", "custom", "K"); !strings.Contains(stderr, "value") {
			t.Errorf("%q: %s", value, stderr)
		}
	}
	if len(b.tokens("web")) != 0 || len(b.engine.Calls) != 0 || exists(b.store().TokensFile(b.env("web").ID)) {
		t.Errorf("a refused token left something: %v, %v", b.tokens("web"), b.engine.Calls)
	}
}

func TestTokenChangesWithoutDockerAreAppliedByUp(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.stdin = "v1"
	b.dialErr = errors.New("no daemon")
	code, out, stderr := b.run("token", "set", "web", "custom", "K")
	if code != 0 || out != "Set K for \"web\"\n" ||
		stderr != "bh: warning: saved but not yet in the environment (no daemon) — bh up web applies it\n" {
		t.Fatalf("exit %d\n%s%s", code, out, stderr)
	}

	b.dialErr = nil
	b.ok("up", "web")
	if string(b.lastRun().Input) != "K=v1\n" {
		t.Errorf("input = %q", b.lastRun().Input)
	}

	// The other way round: a token taken out while the volume could not be
	// written is taken out of it by the next start.
	b.engine.VolumeJobErr = errors.New("no space")
	code, out, stderr = b.run("token", "remove", "web", "K")
	if code != 0 || !strings.Contains(out, "Removed K") ||
		!strings.Contains(stderr, "bh: warning: the environment's volume still has it (writing the tokens into their volume: no space) — bh up web clears it") {
		t.Fatalf("exit %d\n%s%s", code, out, stderr)
	}
	// Until then the start is refused rather than made with the token inside.
	if stderr := b.fails(1, "restart", "web"); !strings.Contains(stderr, "writing the tokens into their volume") {
		t.Errorf("stderr: %s", stderr)
	}
	b.engine.VolumeJobErr = nil
	runs := len(b.engine.VolumeRuns)
	b.ok("up", "web")
	if len(b.engine.VolumeRuns) != runs+1 || len(b.lastRun().Input) != 0 {
		t.Errorf("runs = %+v", b.engine.VolumeRuns[runs:])
	}
}

func TestTokenListShowsWhatItCanWhenARecordIsUnreadable(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	id := b.env("web").ID
	b.stdin = "v1"
	b.ok("token", "set", "web", "custom", "K")
	b.write(b.store().GitHubFile(id), "{oops")
	code, out, stderr := b.run("token", "list", "web")
	if code != 1 || !strings.Contains(out, "K               custom") || !strings.Contains(stderr, "github.json") {
		t.Errorf("exit %d\n%s%s", code, out, stderr)
	}
}

func TestDestroyNamesTheTokensThatStayValid(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.stdin = "hf_secretvalue"
	b.ok("token", "set", "web", "hf", "--no-open")
	b.stdin = "v1"
	b.ok("token", "set", "web", "custom", "K")
	code, out, stderr := b.run("destroy", "web", "--yes")
	want := "bh: warning: tokens are still valid at their services — revoke by hand if needed: HF_TOKEN (https://huggingface.co/settings/tokens), K\n"
	if code != 0 || !strings.Contains(out, `Destroyed "web"`) || !strings.Contains(stderr, want) {
		t.Fatalf("exit %d\n%s%s", code, out, stderr)
	}
	nowhere(t, "hf_secretvalue", out, stderr)
	if len(b.web.Requests()) != 0 {
		t.Errorf("requests = %+v", b.web.Requests())
	}

	// No tokens left, or none ever: nothing to say.
	store := config.Store{Dir: b.dir}
	b.ok("create", "api")
	if note := tokensDestroyNote(store, b.env("api").ID); note != "" {
		t.Errorf("note = %q", note)
	}
	b.stdin = "v1"
	b.ok("token", "set", "api", "custom", "K")
	b.ok("token", "remove", "api", "K")
	if note := tokensDestroyNote(store, b.env("api").ID); note != "" {
		t.Errorf("note = %q", note)
	}
	b.write(store.TokensFile(b.env("api").ID), "{")
	if note := tokensDestroyNote(store, b.env("api").ID); !strings.Contains(note, "could not be read") {
		t.Errorf("note = %q", note)
	}
}
