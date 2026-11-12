package main

import (
	"errors"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/engine/enginetest"
	"github.com/ys319/bulkhead/internal/github/githubtest"
)

const (
	patNew = "github_pat_11AAAAAAA0aaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	patOld = "github_pat_11AAAAAAA0oldoldoldold_cccccccccccccccccccccccccccccccc"
)

func (b *bench) store() config.Store { return config.Store{Dir: b.dir} }

// githubState is the environment's recorded GitHub token; nil for none.
func (b *bench) githubState(name string) *config.GitHubState {
	b.t.Helper()
	state, err := b.store().LoadGitHub(b.env(name).ID)
	if err != nil {
		b.t.Fatal(err)
	}
	return state
}

// seedGitHub records a token as an earlier bh github set would have.
func (b *bench) seedGitHub(name, token string) config.GitHubState {
	b.t.Helper()
	expires := "2026-11-11T06:30:00.000Z"
	state := config.GitHubState{
		Token: token, Preset: "push", Owner: "octo", TokenName: "bh-" + name + "-earlier",
		IssuedAt: "2026-10-12T06:30:00.000Z", ExpiresAt: &expires, Repos: []string{"octo/foo", "octo/bar"},
	}
	if err := b.store().SaveGitHub(b.env(name).ID, state); err != nil {
		b.t.Fatal(err)
	}
	return state
}

// lastRun is the last script run on a volume.
func (b *bench) lastRun() enginetest.VolumeRun {
	b.t.Helper()
	if len(b.engine.VolumeRuns) == 0 {
		b.t.Fatal("no script was run on a volume")
	}
	return b.engine.VolumeRuns[len(b.engine.VolumeRuns)-1]
}

// nowhere fails the test if a secret shows up in what a command printed.
func nowhere(t *testing.T, secret string, outputs ...string) {
	t.Helper()
	for _, output := range outputs {
		if strings.Contains(output, secret) {
			t.Errorf("the secret was printed:\n%s", output)
		}
	}
}

func TestGitHubSet(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	id := b.env("web").ID
	b.web.Accept("octo")
	b.now = time.Date(2026, 10, 12, 15, 30, 0, 0, time.Local)
	b.stdin = "  " + patNew + "\n"

	// The token can be set before the environment was ever started.
	code, out, stderr := b.run("github", "set", "--days", "7", "web", "--preset", "push")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, stderr)
	}
	nowhere(t, patNew, out, stderr)
	if len(b.opened) != 1 || !strings.Contains(out, b.opened[0]) {
		t.Fatalf("opened %v; output:\n%s", b.opened, out)
	}
	page, err := url.Parse(b.opened[0])
	if err != nil {
		t.Fatal(err)
	}
	query := page.Query()
	if page.Host != "github.com" || query.Get("name") != "bh-web-10121530" || query.Get("contents") != "write" ||
		query.Get("expires_in") != "7" || query.Has("target_name") || query.Has("pull_requests") ||
		!strings.Contains(query.Get("description"), `"web"`) {
		t.Errorf("the page: %s", b.opened[0])
	}
	for _, part := range []string{`Set the GitHub token for "web":`, "push · octo · 0 repos · expires in 7d", "repos: (none — only public read access)"} {
		if !strings.Contains(out, part) {
			t.Errorf("output lacks %q:\n%s", part, out)
		}
	}

	state := b.githubState("web")
	want := config.GitHubState{
		Token: patNew, Preset: "push", Owner: "octo", TokenName: "bh-web-10121530",
		IssuedAt: config.Timestamp(b.now), ExpiresAt: ptr(config.Timestamp(b.now.Add(7 * 24 * time.Hour))), Repos: []string{},
	}
	if state == nil || state.Token != want.Token || state.Preset != want.Preset || state.Owner != want.Owner ||
		state.TokenName != want.TokenName || state.IssuedAt != want.IssuedAt || *state.ExpiresAt != *want.ExpiresAt ||
		state.Repos == nil || len(state.Repos) != 0 {
		t.Errorf("state = %+v", state)
	}

	// Into the volume: created with it, written by a script that gets the
	// token on its input, in the holder image.
	run := b.lastRun()
	if !slices.Equal(b.engine.Calls, []string{"volume bulkhead-" + id + "_github", "write-volume bulkhead-" + id + "_github"}) {
		t.Errorf("calls = %v", b.engine.Calls)
	}
	if !strings.HasPrefix(run.Job.Image, "bulkhead-holder:") || run.Job.Target != "/g" || strings.Contains(run.Job.Script, "github_pat_") {
		t.Errorf("job = %+v", run.Job)
	}
	if !strings.HasPrefix(string(run.Input), patNew+"\npreset=push\nowner=octo\nrepos=\nexpires_epoch=") {
		t.Errorf("input = %q", run.Input)
	}

	// Starting the environment writes the volume again, before anything starts.
	b.engine.Calls = nil
	b.ok("up", "web")
	calls := b.engine.Lifecycle()
	if len(calls) < 2 || calls[0] != "write-volume bulkhead-"+id+"_github" || !strings.HasPrefix(calls[1], "create ") {
		t.Errorf("up: %v", calls)
	}
}

func TestGitHubSetUsesWhatGitHubSaysAboutTheToken(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.web.Reply("/user", http.StatusOK, `{"login": "octo"}`, "github-authentication-token-expiration", "2026-10-20 12:00:00 UTC")
	b.web.Reply("/user/repos", http.StatusOK, `[{"full_name": "acme/site", "private": true}]`)
	b.now = time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	b.stdin = patNew

	out := b.ok("github", "set", "web", "--preset", "push+pr", "--owner", "acme", "--no-open")
	if len(b.opened) != 0 {
		t.Errorf("--no-open opened %v", b.opened)
	}
	if !strings.Contains(out, "target_name=acme") || !strings.Contains(out, "pull_requests=write") {
		t.Errorf("the page in the output:\n%s", out)
	}
	// The expiry is GitHub's (an organization may allow less than was
	// asked for), the owner the one that was named.
	if !strings.Contains(out, "push+pr · acme · 1 repo · expires in 8d") || !strings.Contains(out, "repos: acme/site") {
		t.Errorf("output:\n%s", out)
	}
	if state := b.githubState("web"); *state.ExpiresAt != "2026-10-20T12:00:00.000Z" || state.Owner != "acme" {
		t.Errorf("state = %+v", state)
	}
	if input := string(b.lastRun().Input); !strings.Contains(input, "\nrepos=acme/site\n") || !strings.Contains(input, "\nexpires_date=2026-10-20\n") {
		t.Errorf("input = %q", input)
	}
}

func TestGitHubSetReplacesAndRevokesThePreviousToken(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.seedGitHub("web", patOld)
	b.web.Accept("octo")
	// When the old token is revoked, the new one is verified and saved, and
	// the volume is not being written yet: an interruption while it is
	// leaves no token that is valid and unrecorded.
	var savedAtRevoke string
	writesAtRevoke := -1
	b.web.Handle("/credentials/revoke", func(*http.Request) (*http.Response, error) {
		savedAtRevoke = b.githubState("web").Token
		writesAtRevoke = len(b.engine.VolumeRuns)
		return githubtest.Response(http.StatusAccepted, ""), nil
	})
	b.typing, b.secret = true, patNew+"\n"

	code, out, stderr := b.run("github", "set", "web", "--preset", "read")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, stderr)
	}
	nowhere(t, patNew, out, stderr)
	nowhere(t, patOld, out, stderr)
	if !strings.Contains(stderr, "Paste the token (input is hidden): ") {
		t.Errorf("no hidden prompt:\n%s", stderr)
	}
	for _, part := range []string{"(replaces github_pat_...cccc — revoked once the new one is verified)", "revoked the previous token (bh-web-earlier)"} {
		if !strings.Contains(out, part) {
			t.Errorf("output lacks %q:\n%s", part, out)
		}
	}
	revocations := b.web.To("/credentials/revoke")
	if len(revocations) != 1 || !strings.Contains(revocations[0].Body, patOld) || strings.Contains(revocations[0].Body, patNew) {
		t.Errorf("revocations = %+v", revocations)
	}
	if savedAtRevoke != patNew || writesAtRevoke != 0 {
		t.Errorf("at the revocation: saved %q, %d volume writes", savedAtRevoke, writesAtRevoke)
	}
	if !strings.HasPrefix(string(b.lastRun().Input), patNew+"\npreset=read\n") {
		t.Errorf("input = %q", b.lastRun().Input)
	}

	// Pasting the same token again revokes nothing — least of all the
	// token just set — and a clearing left over from a revocation is off.
	if err := b.store().SetGitHubClearPending(b.env("web").ID, true); err != nil {
		t.Fatal(err)
	}
	b.secret = " " + patNew + "\n"
	out = b.ok("github", "set", "web", "--preset", "read")
	if len(b.web.To("/credentials/revoke")) != 1 || strings.Contains(out, "revoked the previous") {
		t.Errorf("the same token was revoked:\n%s", out)
	}
	if b.store().GitHubClearPending(b.env("web").ID) || b.githubState("web").Token != patNew {
		t.Error("the clearing is still pending")
	}

	// A previous token that cannot be revoked does not undo the new one.
	b.seedGitHub("web", patOld)
	b.web.Reply("/credentials/revoke", http.StatusServiceUnavailable, "")
	code, out, stderr = b.run("github", "set", "web", "--preset", "read")
	want := `bh: warning: revoking the previous token failed (HTTP 503) — revoke "bh-web-earlier" by hand at https://github.com/settings/personal-access-tokens`
	if code != 0 || !strings.Contains(stderr, want) || b.githubState("web").Token != patNew {
		t.Errorf("exit %d\n%s%s", code, out, stderr)
	}
}

func TestGitHubSetRefuses(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.ok("remote", "add", "studio.test:22001/main")
	tests := []struct {
		name  string
		code  int
		input string
		args  []string
		want  string
	}{
		// None of these reaches GitHub: b.web knows no path, and any
		// request would fail the test.
		{"a classic token", 1, "ghp_classic", []string{"web", "--preset", "read"}, "fine-grained"},
		// A line break that got through would end up in the metadata.
		{"a second line", 1, "github_pat_ok\nx", []string{"web", "--preset", "read"}, "fine-grained"},
		{"a space inside", 1, "github_pat_ok rest", []string{"web", "--preset", "read"}, "fine-grained"},
		{"something before it", 1, "xgithub_pat_ok", []string{"web", "--preset", "read"}, "fine-grained"},
		{"nothing", 1, "", []string{"web", "--preset", "read"}, "fine-grained"},
		{"a remote environment", 1, patNew, []string{"main", "--preset", "read"}, "remote environment"},
		{"an unknown environment", 1, patNew, []string{"nope", "--preset", "read"}, "no environment named"},
		{"no preset", 2, patNew, []string{"web"}, "--preset read | push | push+pr (no default"},
		{"an unknown preset", 2, patNew, []string{"web", "--preset", "admin"}, "read / push / push+pr (got: admin)"},
		{"zero days", 2, patNew, []string{"web", "--preset", "read", "--days", "0"}, "1-366 (got: 0)"},
		{"too many days", 2, patNew, []string{"web", "--preset", "read", "--days", "367"}, "1-366"},
		{"days with a sign", 2, patNew, []string{"web", "--preset", "read", "--days", "+5"}, "1-366 (got: +5)"},
		{"an owner that is no name", 2, patNew, []string{"web", "--preset", "read", "--owner", "a b"}, "--owner"},
		{"no environment", 2, patNew, []string{"--preset", "read"}, "usage: bh github set"},
		{"two environments", 2, patNew, []string{"web", "api", "--preset", "read"}, "usage: bh github set"},
		{"an unknown flag", 2, patNew, []string{"web", "--preset", "read", "--workflows"}, "not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b.stdin = tt.input
			stderr := b.fails(tt.code, append([]string{"github", "set"}, tt.args...)...)
			if !strings.Contains(stderr, tt.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr, tt.want)
			}
		})
	}

	// A token GitHub turns down is not saved.
	b.web.Reply("/user", http.StatusUnauthorized, "")
	b.stdin = patNew
	if stderr := b.fails(1, "github", "set", "web", "--preset", "read"); !strings.Contains(stderr, "rejected the token") {
		t.Errorf("stderr: %s", stderr)
	}
	if b.githubState("web") != nil || len(b.engine.Calls) != 0 {
		t.Errorf("a refused token left something: %+v, %v", b.githubState("web"), b.engine.Calls)
	}
	for _, sub := range [][]string{{"github"}, {"github", "issue"}, {"github", "status"}, {"github", "revoke"}} {
		if stderr := b.fails(2, sub...); !strings.Contains(stderr, "usage: bh github") {
			t.Errorf("bh %s: %s", strings.Join(sub, " "), stderr)
		}
	}
}

func TestGitHubSetWithoutDockerIsAppliedByUp(t *testing.T) {
	for _, tt := range []struct {
		name string
		fail func(*bench)
		mend func(*bench)
	}{
		{"the engine cannot be reached", func(b *bench) { b.dialErr = errors.New("no daemon") }, func(b *bench) { b.dialErr = nil }},
		{"the write fails", func(b *bench) { b.engine.VolumeJobErr = errors.New("exit status 1:\nno space") }, func(b *bench) { b.engine.VolumeJobErr = nil }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := newBench(t)
			b.ok("create", "web")
			b.web.Accept("octo")
			b.stdin = patNew
			tt.fail(b)
			code, out, stderr := b.run("github", "set", "web", "--preset", "push")
			if code != 0 || !strings.Contains(out, `Set the GitHub token for "web"`) ||
				!strings.Contains(stderr, "bh: warning: the token is saved but not yet in the environment (") ||
				!strings.Contains(stderr, ") — bh up web applies it\n") {
				t.Fatalf("exit %d\n%s%s", code, out, stderr)
			}
			if strings.Count(strings.TrimSpace(stderr), "\n") != 0 {
				t.Errorf("the warning is not one line:\n%s", stderr)
			}
			if b.githubState("web") == nil || len(b.engine.VolumeRuns) != 0 {
				t.Fatalf("state %v, runs %v", b.githubState("web"), b.engine.VolumeRuns)
			}
			tt.mend(b)
			b.ok("up", "web")
			if !strings.HasPrefix(string(b.lastRun().Input), patNew+"\n") {
				t.Errorf("input = %q", b.lastRun().Input)
			}
		})
	}
}

func TestUpStopsWhenATokenCannotBePutInPlace(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	id := b.env("web").ID

	// A record that cannot be read may hold a token that should, or should
	// no longer, be inside.
	b.write(b.store().GitHubFile(id), "{oops")
	if stderr := b.fails(1, "up", "web"); !strings.Contains(stderr, "github.json") || !strings.Contains(stderr, "fix or delete") {
		t.Errorf("stderr: %s", stderr)
	}
	if len(b.engine.Containers) != 0 {
		t.Errorf("containers were created: %v", b.engine.Lifecycle())
	}

	os.Remove(b.store().GitHubFile(id))
	b.seedGitHub("web", patNew)
	b.engine.VolumeJobErr = errors.New("no space")
	if stderr := b.fails(1, "up", "web"); !strings.Contains(stderr, "writing the GitHub token into its volume: no space") {
		t.Errorf("stderr: %s", stderr)
	}
	if len(b.engine.Containers) != 0 {
		t.Errorf("containers were created: %v", b.engine.Lifecycle())
	}
}

func TestGitHubStatus(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	if out := b.ok("github", "status", "web"); !strings.Contains(out, `"web" has no GitHub token (bh github set web --preset read|push|push+pr)`) {
		t.Errorf("output:\n%s", out)
	}
	b.seedGitHub("web", patNew)
	b.now = time.Date(2026, 10, 30, 6, 30, 0, 0, time.UTC)
	out := b.ok("github", "status", "web")
	want := "github: push · octo · 2 repos · expires in 12d\n" +
		"  token:   github_pat_...bbbb  (bh-web-earlier)\n" +
		"  repos:   octo/foo, octo/bar\n" +
		"  issued:  2026-10-12\n" +
		"  expires: 2026-11-11\n"
	if out != want {
		t.Errorf("output:\n%s\nwant:\n%s", out, want)
	}
	nowhere(t, patNew, out)

	b.now = time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	if out := b.ok("github", "status", "web"); !strings.Contains(out, "EXPIRED on 2026-11-11") {
		t.Errorf("output:\n%s", out)
	}
	if len(b.engine.Calls) != 0 || len(b.web.Requests()) != 0 {
		t.Errorf("status went out: %v, %v", b.engine.Calls, b.web.Requests())
	}
}

func TestGitHubSummary(t *testing.T) {
	now := time.Date(2026, 10, 30, 6, 30, 0, 0, time.UTC)
	state := func(edit func(*config.GitHubState)) config.GitHubState {
		expires := "2026-11-11T06:30:00.000Z"
		state := config.GitHubState{Preset: "push", Owner: "octo", ExpiresAt: &expires, Repos: []string{"a/b", "a/c"}}
		edit(&state)
		return state
	}
	tests := []struct {
		edit func(*config.GitHubState)
		want string
	}{
		{func(*config.GitHubState) {}, "push · octo · 2 repos · expires in 12d"},
		{func(s *config.GitHubState) { s.Repos = []string{"a/b"} }, "push · octo · 1 repo · expires in 12d"},
		{func(s *config.GitHubState) { s.Repos = []string{} }, "push · octo · 0 repos · expires in 12d"},
		{func(s *config.GitHubState) { s.Repos = nil }, "push · octo · repos unknown · expires in 12d"},
		{func(s *config.GitHubState) { s.ExpiresAt = nil }, "push · octo · 2 repos · no expiry"},
		{func(s *config.GitHubState) { s.ExpiresAt = ptr("2026-10-29T00:00:00.000Z") }, "push · octo · 2 repos · EXPIRED on 2026-10-29"},
	}
	for _, tt := range tests {
		if got := githubSummary(state(tt.edit), now); got != tt.want {
			t.Errorf("summary = %q, want %q", got, tt.want)
		}
	}
}

func TestGitHubRevoke(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	if stderr := b.fails(1, "github", "revoke", "web", "--yes"); !strings.Contains(stderr, `"web" has no GitHub token`) {
		t.Errorf("stderr: %s", stderr)
	}
	b.ok("up", "web")
	b.seedGitHub("web", patNew)
	id := b.env("web").ID
	volumes := maps.Clone(b.engine.Volumes)
	b.engine.Calls = nil

	// Asked first; without a terminal to ask on, --yes says it.
	if stderr := b.fails(1, "github", "revoke", "web"); !strings.Contains(stderr, "--yes") {
		t.Errorf("stderr: %s", stderr)
	}
	b.typing, b.stdin = true, "n\n"
	stderr := b.fails(1, "github", "revoke", "web")
	if !strings.Contains(stderr, `Revoke github_pat_...bbbb (bh-web-earlier) on GitHub and remove it from "web"? [y/N]`) {
		t.Errorf("stderr: %s", stderr)
	}
	if len(b.web.Requests()) != 0 || b.githubState("web") == nil {
		t.Fatal("an unconfirmed revoke did something")
	}

	// If GitHub does not confirm, nothing is removed: the copy here is
	// what a retry needs.
	b.web.Reply("/credentials/revoke", http.StatusInternalServerError, "")
	stderr = b.fails(1, "github", "revoke", "web", "--yes")
	if !strings.Contains(stderr, "revoking the token failed (HTTP 500) — nothing was removed") || !strings.Contains(stderr, `revoke "bh-web-earlier" by hand`) {
		t.Errorf("stderr: %s", stderr)
	}
	if b.githubState("web") == nil || len(b.engine.Calls) != 0 {
		t.Fatalf("a failed revoke removed something: %v", b.engine.Calls)
	}

	b.web.Revocable()
	b.stdin = "y\n"
	out := b.ok("github", "revoke", "web")
	if !strings.Contains(out, `Revoked the GitHub token of "web" (bh-web-earlier)`) {
		t.Errorf("output:\n%s", out)
	}
	if b.githubState("web") != nil || b.store().GitHubClearPending(id) {
		t.Error("the record is still there, or a clearing is pending")
	}
	if revocations := b.web.To("/credentials/revoke"); !strings.Contains(revocations[len(revocations)-1].Body, patNew) {
		t.Errorf("revocations = %+v", revocations)
	}
	// The volume is emptied, by a script without input. It is not removed.
	run := b.lastRun()
	if run.Job.Script != "rm -f /g/token /g/meta" || run.Job.Volume != "bulkhead-"+id+"_github" || run.Input != nil {
		t.Errorf("run = %+v", run)
	}
	b.noVolumeRemoved(volumes)
}

func TestGitHubRevokeThatCannotEmptyTheVolumeLeavesItToUp(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.seedGitHub("web", patNew)
	id := b.env("web").ID
	b.web.Revocable()
	b.dialErr = errors.New("no daemon")

	code, out, stderr := b.run("github", "revoke", "web", "--yes")
	if code != 0 || !strings.Contains(out, "Revoked the GitHub token") ||
		!strings.Contains(stderr, "bh: warning: the revoked token is still in the environment's volume (no daemon) — bh up web clears it") {
		t.Fatalf("exit %d\n%s%s", code, out, stderr)
	}
	if b.githubState("web") != nil || !b.store().GitHubClearPending(id) {
		t.Fatal("the record is still there, or no clearing is pending")
	}

	// The next start empties the volume, once.
	b.dialErr = nil
	b.ok("up", "web")
	if run := b.lastRun(); run.Job.Script != "rm -f /g/token /g/meta" || len(b.engine.VolumeRuns) != 1 {
		t.Errorf("runs = %+v", b.engine.VolumeRuns)
	}
	if b.store().GitHubClearPending(id) {
		t.Error("the clearing is still pending")
	}
	b.ok("up", "web")
	if len(b.engine.VolumeRuns) != 1 {
		t.Errorf("runs = %+v", b.engine.VolumeRuns)
	}
	for _, call := range b.engine.Calls {
		if strings.HasPrefix(call, "rm-volume") {
			t.Errorf("a volume was removed: %s", call)
		}
	}
}

func TestDestroyRevokesTheGitHubToken(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.ok("up", "web")
	state := b.seedGitHub("web", patNew)
	id := b.env("web").ID
	// Revoked once Docker's part is done — if that fails the environment
	// stays, and so must its token — and while the record is still there.
	var callsAtRevoke []string
	recordAtRevoke := false
	b.web.Handle("/credentials/revoke", func(*http.Request) (*http.Response, error) {
		callsAtRevoke = slices.Clone(b.engine.Calls)
		recordAtRevoke = exists(b.store().GitHubFile(id))
		return githubtest.Response(http.StatusAccepted, ""), nil
	})

	// The question says that the token goes too.
	b.stdin = "no\n"
	if stderr := b.fails(1, "destroy", "web"); !strings.Contains(stderr, "Its GitHub token ("+state.TokenName+") is revoked on GitHub.\nType the name to confirm: ") {
		t.Errorf("stderr: %s", stderr)
	}
	if len(b.web.Requests()) != 0 {
		t.Fatal("an unconfirmed destroy revoked the token")
	}

	code, out, stderr := b.run("destroy", "web", "--yes")
	if code != 0 || !strings.Contains(out, `Destroyed "web"`) || strings.Contains(stderr, "warning") {
		t.Fatalf("exit %d\n%s%s", code, out, stderr)
	}
	revocations := b.web.To("/credentials/revoke")
	if len(revocations) != 1 || !strings.Contains(revocations[0].Body, patNew) {
		t.Errorf("revocations = %+v", revocations)
	}
	if !slices.Contains(callsAtRevoke, "rm-volume bulkhead-"+id+"_github") || !slices.Contains(callsAtRevoke, "rm-network bulkhead-"+id) || !recordAtRevoke {
		t.Errorf("at the revocation: calls %v, record there: %v", callsAtRevoke, recordAtRevoke)
	}
	if exists(b.store().EnvDir(id)) {
		t.Error("the settings are still there")
	}
}

func TestDestroyGoesOnWhenTheTokenCannotBeRevoked(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.seedGitHub("web", patNew)
	id := b.env("web").ID
	b.web.Reply("/credentials/revoke", http.StatusServiceUnavailable, "")
	code, out, stderr := b.run("destroy", "web", "--yes")
	want := `bh: warning: revoking the GitHub token failed (HTTP 503) — revoke "bh-web-earlier" by hand at https://github.com/settings/personal-access-tokens`
	if code != 0 || !strings.Contains(out, `Destroyed "web"`) || !strings.Contains(stderr, want) {
		t.Fatalf("exit %d\n%s%s", code, out, stderr)
	}
	if exists(b.store().EnvDir(id)) {
		t.Error("the settings are still there")
	}

	// A record that cannot be read: nothing to revoke with, and it says so.
	b.ok("create", "api")
	id = b.env("api").ID
	b.write(b.store().GitHubFile(id), "{oops")
	before := len(b.web.Requests())
	code, out, stderr = b.run("destroy", "api", "--yes")
	if code != 0 || !strings.Contains(stderr, "could not be read, so the token was not revoked — check https://github.com/settings/personal-access-tokens") {
		t.Fatalf("exit %d\n%s%s", code, out, stderr)
	}
	if len(b.web.Requests()) != before || exists(b.store().EnvDir(id)) {
		t.Error("a request was made, or the settings are still there")
	}

	// A remote environment has no tokens here; nothing goes out.
	b.ok("remote", "add", "studio.test:22001/main")
	b.ok("destroy", "main", "--yes")
	if len(b.web.Requests()) != before {
		t.Error("destroying a remote environment made a request")
	}
}

func TestMigratedTokenRecordsAreManagedLikeAnyOther(t *testing.T) {
	b := newBench(t)
	dir := b.legacyEnv("web", legacyWeb)
	// What the TypeScript version left: its files, byte for byte.
	b.write(filepath.Join(dir, "github.json"), `{
  "token": "`+patOld+`",
  "preset": "push",
  "owner": "octo",
  "tokenName": "bh-web-10121530",
  "issuedAt": "2026-10-12T06:30:00.000Z",
  "expiresAt": "2026-11-11T06:30:00.000Z",
  "repos": null
}
`)
	b.write(filepath.Join(dir, "tokens.json"), `{
  "tokens": [
    {
      "kind": "hf",
      "name": "HF_TOKEN",
      "value": "hf_secretvalue",
      "addedAt": "2026-10-12T06:30:00.000Z"
    }
  ]
}
`)
	b.engine.AddLegacy("web", false)
	volumes := maps.Clone(b.engine.Volumes)
	out := b.ok("migrate", "web", "--yes")
	if strings.Contains(out, "not coming along") {
		t.Errorf("migrate still says something stays behind:\n%s", out)
	}

	b.now = time.Date(2026, 10, 30, 6, 30, 0, 0, time.UTC)
	out = b.ok("token", "list", "web")
	if !strings.Contains(out, "github · push · octo · repos unknown · expires in 12d") || !strings.Contains(out, "HF_TOKEN") {
		t.Errorf("list:\n%s", out)
	}
	b.web.Revocable()
	b.ok("github", "revoke", "web", "--yes")
	b.ok("token", "remove", "web", "HF_TOKEN")
	if b.githubState("web") != nil {
		t.Error("the record is still there")
	}
	if run := b.lastRun(); run.Job.Volume != "bulkhead-web_tokens" || len(run.Input) != 0 {
		t.Errorf("run = %+v", run)
	}
	b.noVolumeRemoved(volumes)
}

func TestScanHiddenLine(t *testing.T) {
	tests := []struct {
		name, keys, want string
		fails            bool
	}{
		{"up to return", "secret\rrest", "secret", false},
		{"up to a line feed", "secret\n", "secret", false},
		{"the end of the input", "secret", "secret", false},
		{"backspace", "sex\x7fcrex\x08t", "secret", false},
		{"backspace on nothing", "\x7f\x7fok\r", "ok", false},
		{"a paste with its marks", "\x1b[200~secret\x1b[201~\r", "secret", false},
		{"Ctrl-D on an empty line", "\x04", "", false},
		{"Ctrl-C", "sec\x03ret\r", "", true},
		{"longer than a terminal's line buffer", strings.Repeat("a", 5000) + "\r", strings.Repeat("a", 5000), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := scanHiddenLine(strings.NewReader(tt.keys))
			if got != tt.want || (err != nil) != tt.fails {
				t.Errorf("got %q, %v", got, err)
			}
		})
	}
}
