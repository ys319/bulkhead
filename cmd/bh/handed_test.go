package main

import (
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ys319/bulkhead/internal/config"
)

const (
	patNew  = "github_pat_11AAAAAAA0aaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	patNext = "github_pat_11AAAAAAA0cccccccccccc_dddddddddddddddddddddddddddddddddddd"
	hfToken = "hf_abcdefghijklmnopqrstuvwxyz012345"
	hfNext  = "hf_zyxwvutsrqponmlkjihgfedcba543210"
)

func (b *bench) store() config.Store { return config.Store{Dir: b.dir} }

// tokens are the records of the environment's tokens.
func (b *bench) tokens(name string) []config.Token {
	b.t.Helper()
	tokens, err := b.store().LoadTokens(b.env(name).ID)
	if err != nil {
		b.t.Fatal(err)
	}
	return tokens
}

func (b *bench) tokenOf(name, service string) *config.Token {
	b.t.Helper()
	for _, token := range b.tokens(name) {
		if token.Service == service {
			return &token
		}
	}
	return nil
}

func (b *bench) githubState(name string) *config.Token { return b.tokenOf(name, config.ServiceGitHub) }

// kept is the value in the stand-in keychain for the environment's key;
// empty when there is none.
func (b *bench) kept(id, key string) string {
	b.t.Helper()
	owner, err := b.store().Owner()
	if err != nil {
		b.t.Fatal(err)
	}
	return b.keychain[owner+"/"+id+"/"+key]
}

// seedGitHub gives the environment a GitHub token the way the user does.
func (b *bench) seedGitHub(name, token string) {
	b.t.Helper()
	b.web.Accept("octo")
	b.stdin = token
	b.ok("token", "set", name, "github", "push", "--no-open")
	b.stdin = ""
}

func (b *bench) seedHuggingFace(name, token string) {
	b.t.Helper()
	b.web.HuggingFace("me", "read")
	b.stdin = token
	b.ok("token", "set", name, "huggingface", "--no-open")
	b.stdin = ""
}

// writes are what was written into the environment's volume of the key
// (github or tokens), oldest first.
func (b *bench) writes(key string) []string {
	var inputs []string
	for _, run := range b.engine.VolumeRuns {
		if strings.HasSuffix(run.Job.Volume, "_"+key) && run.Input != nil {
			inputs = append(inputs, string(run.Input))
		}
	}
	return inputs
}

// lastEnvWrite is the variables' file as it was last written; "(none)"
// when it never was.
func (b *bench) lastEnvWrite() string {
	writes := b.writes("tokens")
	if len(writes) == 0 {
		return "(none)"
	}
	return writes[len(writes)-1]
}

func TestVarsAreSetShownAndHandedIn(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	if out := b.ok("var", "list", "web"); !strings.Contains(out, "has no variables") {
		t.Errorf("empty list: %s", out)
	}

	out := b.ok("var", "set", "web", "GIT_AUTHOR_NAME", "It's Me (work)")
	if !strings.Contains(out, `Set GIT_AUTHOR_NAME for "web" — new shells see it`) {
		t.Errorf("output: %s", out)
	}
	b.ok("var", "set", "web", "http_proxy", "http://proxy:8080")
	if out := b.ok("var", "set", "web", "http_proxy", "http://other:8080"); !strings.Contains(out, "Replaced http_proxy") {
		t.Errorf("replace: %s", out)
	}
	// Shown with their values, in order; kept with the settings.
	if list := b.ok("var", "list", "web"); list != "GIT_AUTHOR_NAME=It's Me (work)\nhttp_proxy=http://other:8080\n" {
		t.Errorf("list:\n%s", list)
	}
	if vars := b.env("web").Vars; len(vars) != 2 || vars["GIT_AUTHOR_NAME"] != "It's Me (work)" {
		t.Errorf("env.json vars = %v", vars)
	}
	// In the environment's file as they are; bh-guest quotes them.
	if got := b.lastEnvWrite(); got != "GIT_AUTHOR_NAME=It's Me (work)\nhttp_proxy=http://other:8080\n" {
		t.Errorf("the environment got %q", got)
	}
	// A variable is no secret: the keychain is not asked, and nothing is
	// decided about it.
	if len(b.keychain) != 0 {
		t.Errorf("keychain: %v", b.keychain)
	}
	if kind, _ := b.store().SecretStore(); kind != "" {
		t.Errorf("secret store decided: %q", kind)
	}

	// A copy of the environment has them.
	b.ok("create", "copy", "--from", "web")
	if vars := b.env("copy").Vars; vars["http_proxy"] != "http://other:8080" {
		t.Errorf("the copy's vars = %v", vars)
	}

	b.ok("var", "unset", "web", "http_proxy")
	b.ok("var", "unset", "web", "GIT_AUTHOR_NAME")
	if got := b.lastEnvWrite(); got != "" {
		t.Errorf("after the last unset the environment got %q", got)
	}
	if err := b.fails(1, "var", "unset", "web", "NOPE"); !strings.Contains(err, "has no variable NOPE") {
		t.Errorf("unset of nothing: %s", err)
	}

	for name, part := range map[string]string{"GH_TOKEN": "bh token set", "PATH": "reserved", "9X": "letters, digits"} {
		if err := b.fails(1, "var", "set", "web", name, "x"); !strings.Contains(err, part) {
			t.Errorf("var set %s: %s", name, err)
		}
	}
	if err := b.fails(1, "var", "set", "web", "OK", "two\nlines"); !strings.Contains(err, "one line") {
		t.Errorf("two lines: %s", err)
	}
	b.ok("remote", "add", "studio.test:22001/far")
	if err := b.fails(1, "var", "set", "far", "A", "b"); !strings.Contains(err, "remote environment") {
		t.Errorf("remote: %s", err)
	}
}

func TestSecretsAreKeptOutOfFiles(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	id := b.env("web").ID
	b.stdin = "s3cret with spaces & 'quotes'\n"
	out := b.ok("secret", "set", "web", "API_KEY")
	if !strings.Contains(out, `Set the secret API_KEY for "web"`) || strings.Contains(out, "s3cret") {
		t.Errorf("output: %s", out)
	}
	if got := b.kept(id, "secret/API_KEY"); got != "s3cret with spaces & 'quotes'" {
		t.Errorf("kept %q", got)
	}
	if kind, _ := b.store().SecretStore(); kind != "keychain" {
		t.Errorf("secret store = %q", kind)
	}
	// Nowhere in the configuration directory.
	err := filepath.Walk(b.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || strings.HasPrefix(filepath.Base(path), "id_ed25519") {
			return err
		}
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), "s3cret") {
			t.Errorf("%s holds the value", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := b.lastEnvWrite(); got != "API_KEY=s3cret with spaces & 'quotes'\n" {
		t.Errorf("the environment got %q", got)
	}
	if list := b.ok("secret", "list", "web"); list != "API_KEY\n" {
		t.Errorf("list: %q", list)
	}

	// One name, one route.
	if err := b.fails(1, "var", "set", "web", "API_KEY", "x"); !strings.Contains(err, "is a secret") {
		t.Errorf("a variable of the secret's name: %s", err)
	}
	b.ok("var", "set", "web", "PLAIN", "x")
	b.stdin = "v"
	if err := b.fails(1, "secret", "set", "web", "PLAIN"); !strings.Contains(err, "is a variable") {
		t.Errorf("a secret of the variable's name: %s", err)
	}
	if err := b.fails(1, "secret", "set", "web", "HF_TOKEN"); !strings.Contains(err, "bh token set") {
		t.Errorf("a secret named like a token: %s", err)
	}
	b.stdin = ""
	if err := b.fails(1, "secret", "set", "web", "EMPTY"); !strings.Contains(err, "empty") {
		t.Errorf("an empty value: %s", err)
	}

	// Not copied with the settings.
	b.ok("create", "copy", "--from", "web")
	if list := b.ok("secret", "list", "copy"); !strings.Contains(list, "has no secrets") {
		t.Errorf("the copy's secrets: %s", list)
	}

	b.ok("secret", "remove", "web", "API_KEY")
	if b.kept(id, "secret/API_KEY") != "" || !strings.Contains(b.lastEnvWrite(), "PLAIN=x") || strings.Contains(b.lastEnvWrite(), "API_KEY") {
		t.Errorf("after remove: kept %q, environment %q", b.kept(id, "secret/API_KEY"), b.lastEnvWrite())
	}
	if err := b.fails(1, "secret", "remove", "web", "API_KEY"); !strings.Contains(err, "has no secret") {
		t.Errorf("remove twice: %s", err)
	}
}

func TestSecretStoreIsChosenNotFallenBackOn(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	id := b.env("web").ID
	if out := b.ok("secret", "store"); !strings.Contains(out, "not decided yet") {
		t.Errorf("before anything: %s", out)
	}

	// No credential store here (as over ssh): nothing is written to a
	// file behind the user's back.
	b.noKeychain = true
	b.stdin = "value-1"
	if err := b.fails(1, "secret", "set", "web", "API_KEY"); !strings.Contains(err, "cannot be used") || !strings.Contains(err, "bh secret store file") {
		t.Errorf("without a keychain: %s", err)
	}
	if _, err := os.Stat(b.store().ValuesFile(id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a values file appeared: %v", err)
	}
	if err := b.fails(1, "secret", "store", "keychain"); !strings.Contains(err, "cannot be used") {
		t.Errorf("choosing the keychain without one: %s", err)
	}

	b.ok("secret", "store", "file")
	b.ok("secret", "set", "web", "API_KEY")
	info, err := os.Stat(b.store().ValuesFile(id))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the values file: %v %v", info, err)
	}
	if len(b.keychain) != 0 {
		t.Errorf("keychain: %v", b.keychain)
	}
	if out := b.ok("secret", "store"); !strings.Contains(out, "(file)") {
		t.Errorf("store: %s", out)
	}
	// The records still hold no values.
	if data, _ := os.ReadFile(b.store().SecretsFile(id)); strings.Contains(string(data), "value-1") {
		t.Errorf("secrets.json holds the value:\n%s", data)
	}

	// The keychain becomes usable: the values move, and leave the file.
	b.noKeychain = false
	if out := b.ok("secret", "store", "keychain"); !strings.Contains(out, "1 moved there") {
		t.Errorf("moving: %s", out)
	}
	if b.kept(id, "secret/API_KEY") != "value-1" {
		t.Errorf("keychain after the move: %v", b.keychain)
	}
	if _, err := os.Stat(b.store().ValuesFile(id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the values file stayed: %v", err)
	}
	b.ok("up", "web")
	if got := b.lastEnvWrite(); got != "API_KEY=value-1\n" {
		t.Errorf("the environment got %q", got)
	}

	// For scripts: the variable decides, whatever the setting.
	other := newBench(t)
	other.environ = map[string]string{"BH_SECRET_STORE": "file"}
	other.noKeychain = true
	other.ok("create", "web")
	other.stdin = "v"
	other.ok("secret", "set", "web", "K")
	if _, err := os.Stat(other.store().ValuesFile(other.env("web").ID)); err != nil {
		t.Errorf("BH_SECRET_STORE=file: %v", err)
	}
}

func TestTokenSetGitHub(t *testing.T) {
	b := newBench(t)
	b.now = time.Date(2026, 10, 12, 15, 30, 0, 0, time.UTC)
	b.ok("create", "web")
	id := b.env("web").ID
	b.web.Accept("octo", "github-authentication-token-expiration", "2026-11-11 15:30:00 UTC")
	b.stdin = patNew + "\n"

	code, out, stderr := b.run("token", "set", "web", "github", "push", "--days", "30")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, stderr)
	}
	// The page is opened filled in: the name, the preset's permissions —
	// reading CI comes with push — and never workflows.
	if len(b.opened) != 1 {
		t.Fatalf("opened %v", b.opened)
	}
	page := b.opened[0]
	for _, part := range []string{"personal-access-tokens/new?", "name=bh-web-10121530", "contents=write", "actions=read", "statuses=read", "expires_in=30"} {
		if !strings.Contains(page, part) {
			t.Errorf("the page lacks %s: %s", part, page)
		}
	}
	for _, part := range []string{"pull_requests", "workflows"} {
		if strings.Contains(page, part) {
			t.Errorf("the page asks for %s: %s", part, page)
		}
	}
	if !strings.Contains(out, page) || !strings.Contains(out, `Set the GitHub token of "web": push · octo · 0 repos · expires in 30d`) || strings.Contains(out+stderr, patNew) {
		t.Errorf("output:\n%s%s", out, stderr)
	}

	token := b.githubState("web")
	if token == nil || token.Preset != "push" || token.Owner != "octo" || token.TokenName != "bh-web-10121530" || token.ExpiresAt == nil {
		t.Fatalf("record = %+v", token)
	}
	if b.kept(id, "token/github") != patNew {
		t.Errorf("keychain = %v", b.keychain)
	}
	if data, _ := os.ReadFile(b.store().TokensFile(id)); strings.Contains(string(data), "github_pat_") {
		t.Errorf("tokens.json holds the token:\n%s", data)
	}
	if _, err := os.Stat(filepath.Join(b.store().EnvDir(id), "github.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a github.json appeared: %v", err)
	}
	// Into the volume: the token, then what the environment is told.
	writes := b.writes("github")
	if len(writes) != 1 || !strings.HasPrefix(writes[0], patNew+"\npreset=push\nowner=octo\nrepos=\nexpires_epoch=") {
		t.Errorf("github volume: %q", writes)
	}

	if list := b.ok("token", "list", "web"); !strings.Contains(list, "github") || !strings.Contains(list, "push · octo") ||
		!strings.Contains(list, "name:    bh-web-10121530") || !strings.Contains(list, "expires: 2026-11-11") {
		t.Errorf("list:\n%s", list)
	}

	// contribute adds pull requests.
	b.opened = nil
	b.stdin = patNext
	b.web.Revocable()
	out = b.ok("token", "set", "web", "github", "contribute", "--owner", "acme")
	if !strings.Contains(b.opened[0], "pull_requests=write") || !strings.Contains(b.opened[0], "target_name=acme") {
		t.Errorf("contribute page: %s", b.opened[0])
	}
	// The one it replaces is revoked, after the new one is in.
	if !strings.Contains(out, "revoked the previous value") || len(b.web.To("/credentials/revoke")) != 1 ||
		!strings.Contains(b.web.To("/credentials/revoke")[0].Body, patNew) {
		t.Errorf("replace:\n%s\n%v", out, b.web.To("/credentials/revoke"))
	}
	if got := b.githubState("web"); got.Preset != "contribute" || got.Owner != "acme" || b.kept(id, "token/github") != patNext {
		t.Errorf("after the replace: %+v", got)
	}
}

func TestTokenSetRefuses(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.web.Accept("octo")
	for _, test := range []struct {
		why   string
		code  int
		stdin string
		args  []string
		part  string
	}{
		{"an unknown preset", 2, patNew, []string{"web", "github", "admin"}, "read | push | contribute"},
		{"the old preset name", 2, patNew, []string{"web", "github", "push+pr"}, "read | push | contribute"},
		{"an unknown service", 2, patNew, []string{"web", "gitlab"}, "github | huggingface"},
		{"a preset for a service without them", 2, hfToken, []string{"web", "huggingface", "read"}, "has no presets"},
		{"a classic token", 1, "ghp_classic", []string{"web", "github", "read"}, "fine-grained"},
		{"something that is not a token", 1, "hello", []string{"web", "huggingface"}, "hf_"},
		{"days out of range", 2, patNew, []string{"web", "github", "read", "--days", "400"}, "--days takes 1-366"},
		{"too many words", 2, patNew, []string{"web", "github", "read", "more"}, "usage: bh token set"},
	} {
		b.stdin = test.stdin
		if err := b.fails(test.code, append([]string{"token", "set", "--no-open"}, test.args...)...); !strings.Contains(err, test.part) {
			t.Errorf("%s: %s", test.why, err)
		}
	}
	if len(b.tokens("web")) != 0 || len(b.keychain) != 0 {
		t.Errorf("something was kept: %v %v", b.tokens("web"), b.keychain)
	}
	// Nothing that failed its form went out to a service.
	for _, request := range b.web.Requests() {
		if strings.Contains(request.Authorization, "ghp_") || strings.Contains(request.Authorization, "hello") {
			t.Errorf("sent out: %+v", request)
		}
	}
	// A token GitHub does not accept is not kept.
	b.web.Reply("/user", http.StatusUnauthorized, `{"message": "Bad credentials"}`)
	b.stdin = patNew
	b.fails(1, "token", "set", "web", "github", "read", "--no-open")
	if len(b.tokens("web")) != 0 || len(b.keychain) != 0 {
		t.Errorf("a rejected token was kept: %v", b.keychain)
	}
}

func TestTokenStepsAreAskedForWhenLeftOut(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.ok("create", "api")
	b.web.Accept("octo")

	// Away from a terminal, what is missing is named.
	b.stdin = patNew
	for args, missing := range map[string]string{"": "the environment", "web": "the service", "web github": "the preset"} {
		words := append([]string{"token", "set", "--no-open"}, strings.Fields(args)...)
		if err := b.fails(2, words...); !strings.Contains(err, missing+" is missing") && !strings.Contains(err, missing+" (") {
			t.Errorf("bh token set %s: %s", args, err)
		}
	}

	// At a terminal each missing step is asked for, in order: environment,
	// service, preset — and, once a conversation is going, the owner.
	b.typing, b.secret = true, patNew
	b.answers = []string{"api", "github", "contribute"}
	b.stdin = "acme\n"
	b.offered = nil
	out := b.ok("token", "set", "--no-open")
	if !slices.Equal(b.offered, []string{"api", "web", "github", "huggingface", "read", "push", "contribute"}) {
		t.Errorf("offered %v", b.offered)
	}
	if got := b.tokenOf("api", "github"); got == nil || got.Preset != "contribute" || got.Owner != "acme" {
		t.Errorf("record = %+v\n%s", got, out)
	}

	// Given the environment, the rest is asked.
	b.answers, b.offered, b.stdin = []string{"github", "read"}, nil, "\n"
	b.ok("token", "set", "web", "--no-open")
	if !slices.Equal(b.offered, []string{"github", "huggingface", "read", "push", "contribute"}) {
		t.Errorf("offered %v", b.offered)
	}
	if got := b.tokenOf("web", "github"); got == nil || got.Preset != "read" || got.Owner != "octo" {
		t.Errorf("record = %+v", got)
	}

	// Everything given: nothing is asked, the owner included.
	b.web.Revocable()
	b.secret, b.offered, b.stdin = patNext, nil, ""
	b.ok("token", "set", "web", "github", "push", "--no-open")
	if len(b.offered) != 0 || b.tokenOf("web", "github").Owner != "octo" {
		t.Errorf("asked %v, owner %q", b.offered, b.tokenOf("web", "github").Owner)
	}

	// A service without presets has no third question.
	b.web.HuggingFace("me", "write")
	b.secret, b.answers, b.offered = hfToken, []string{"huggingface"}, nil
	b.ok("token", "set", "web", "--no-open")
	if !slices.Equal(b.offered, []string{"github", "huggingface"}) {
		t.Errorf("offered %v", b.offered)
	}
	if got := b.tokenOf("web", "huggingface"); got == nil || got.Access != "write" || got.Owner != "me" || got.Preset != "" {
		t.Errorf("record = %+v", got)
	}
}

func TestTokenRefreshKeepsWhatWasAskedFor(t *testing.T) {
	b := newBench(t)
	b.now = time.Date(2026, 10, 12, 15, 30, 0, 0, time.UTC)
	b.ok("create", "web")
	id := b.env("web").ID
	if err := b.fails(1, "token", "refresh", "web", "--no-open"); !strings.Contains(err, "no tokens to refresh") {
		t.Errorf("nothing to refresh: %s", err)
	}
	b.web.Accept("octo", "github-authentication-token-expiration", "2026-11-11 15:30:00 UTC")
	b.stdin = patNew
	b.ok("token", "set", "web", "github", "contribute", "--owner", "acme", "--no-open")
	before := *b.githubState("web")

	// A month on: the same token, regenerated on GitHub's page.
	b.now = b.now.Add(29 * 24 * time.Hour)
	if list, stderr := b.ok("token", "list", "web"), ""; !strings.Contains(list, "expires in 1d") {
		t.Errorf("list: %s%s", list, stderr)
	}
	_, _, stderr := b.run("token", "list")
	if !strings.Contains(stderr, "expires soon — bh token refresh web github") {
		t.Errorf("no warning about the expiry: %s", stderr)
	}

	// The same value pasted again is not a refresh.
	if err := b.fails(1, "token", "refresh", "web", "github", "--no-open"); !strings.Contains(err, "already has") {
		t.Errorf("the same value: %s", err)
	}

	b.web.Accept("octo", "github-authentication-token-expiration", "2026-12-10 15:30:00 UTC")
	b.web.Revocable()
	b.stdin, b.opened = patNext, nil
	out := b.ok("token", "refresh", "web")
	// With one token there is nothing to ask; the page is the list, and
	// the token is named.
	if len(b.opened) != 1 || b.opened[0] != "https://github.com/settings/personal-access-tokens" ||
		!strings.Contains(out, `open the token "bh-web-10121530" and choose "Regenerate token"`) {
		t.Errorf("refresh:\n%s\nopened %v", out, b.opened)
	}
	after := *b.githubState("web")
	if after.Preset != before.Preset || after.Owner != before.Owner || after.TokenName != before.TokenName {
		t.Errorf("what was asked for changed: %+v", after)
	}
	if left, _ := after.DaysLeft(b.now); left != 30 || b.kept(id, "token/github") != patNext {
		t.Errorf("after the refresh: %d days, kept %q", left, b.kept(id, "token/github"))
	}
	if !strings.Contains(out, `Refreshed the GitHub token of "web": contribute · acme`) {
		t.Errorf("output:\n%s", out)
	}
	if err := b.fails(2, "token", "refresh", "web", "github", "--owner", "x"); !strings.Contains(err, "usage: bh token refresh") {
		t.Errorf("refresh with --owner: %s", err)
	}
}

func TestHuggingFaceTokens(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	id := b.env("web").ID
	b.web.HuggingFace("me", "read")
	b.stdin = hfToken + "\n"
	out := b.ok("token", "set", "web", "hf")
	// Its page is opened as it is: the type is chosen there.
	if !slices.Equal(b.opened, []string{"https://huggingface.co/settings/tokens"}) ||
		!strings.Contains(out, `Set the Hugging Face token of "web": read · me · no expiry`) {
		t.Errorf("output:\n%s\nopened %v", out, b.opened)
	}
	if b.kept(id, "token/huggingface") != hfToken || b.lastEnvWrite() != "HF_TOKEN="+hfToken+"\n" {
		t.Errorf("kept %v, environment %q", b.keychain, b.lastEnvWrite())
	}

	// Hugging Face cannot be asked: the token is taken as given.
	b.web.Reply("/api/whoami-v2", http.StatusBadGateway, "")
	b.web.Reply("/api/credentials/revoke", http.StatusAccepted, "")
	b.stdin = hfNext
	code, out, stderr := b.run("token", "set", "web", "huggingface", "--no-open")
	if code != 0 || !strings.Contains(out, "could not be asked about it (HTTP 502) — taken as given") {
		t.Errorf("exit %d\n%s%s", code, out, stderr)
	}
	// The value it replaced is revoked: Hugging Face has a way to.
	if revoked := b.web.To("/api/credentials/revoke"); len(revoked) != 1 || !strings.Contains(revoked[0].Body, hfToken) {
		t.Errorf("revocations: %v", revoked)
	}
	// One it does not accept is not kept.
	b.web.Reply("/api/whoami-v2", http.StatusUnauthorized, "")
	b.stdin = hfToken
	if err := b.fails(1, "token", "set", "web", "huggingface", "--no-open"); !strings.Contains(err, "does not accept") {
		t.Errorf("a rejected token: %s", err)
	}
	if b.kept(id, "token/huggingface") != hfNext {
		t.Errorf("kept %q", b.kept(id, "token/huggingface"))
	}

	b.ok("token", "remove", "web", "huggingface", "--yes")
	if len(b.web.To("/api/credentials/revoke")) != 2 || b.tokenOf("web", "huggingface") != nil || b.kept(id, "token/huggingface") != "" || b.lastEnvWrite() != "" {
		t.Errorf("after remove: %v, environment %q", b.keychain, b.lastEnvWrite())
	}
}

func TestTokenRemoveRevokes(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	id := b.env("web").ID
	if err := b.fails(1, "token", "remove", "web", "github", "--yes"); !strings.Contains(err, "has no tokens") {
		t.Errorf("nothing to remove: %s", err)
	}
	b.ok("up", "web")
	b.seedGitHub("web", patNew)
	volumes := maps.Clone(b.engine.Volumes)

	// Asked first; away from a terminal, --yes says it.
	if err := b.fails(1, "token", "remove", "web", "github"); !strings.Contains(err, "--yes") {
		t.Errorf("without --yes: %s", err)
	}
	b.typing, b.stdin = true, "n\n"
	if err := b.fails(1, "token", "remove", "web"); !strings.Contains(err, `Revoke the GitHub token of "web"`) {
		t.Errorf("the question: %s", err)
	}
	if len(b.web.To("/credentials/revoke")) != 0 || b.githubState("web") == nil {
		t.Fatal("an unconfirmed remove did something")
	}

	// If GitHub does not confirm, nothing is removed: the copy here is
	// what a retry needs.
	b.web.Reply("/credentials/revoke", http.StatusInternalServerError, "")
	if err := b.fails(1, "token", "remove", "web", "github", "--yes"); !strings.Contains(err, "revoking the token failed (HTTP 500) — nothing was removed") {
		t.Errorf("a failed revocation: %s", err)
	}
	if b.githubState("web") == nil || b.kept(id, "token/github") != patNew {
		t.Fatal("a failed revocation removed something")
	}

	b.web.Revocable()
	b.stdin = "y\n"
	out := b.ok("token", "remove", "web")
	if !strings.Contains(out, `Revoked the GitHub token of "web" and took it out`) {
		t.Errorf("output: %s", out)
	}
	if b.githubState("web") != nil || b.kept(id, "token/github") != "" {
		t.Error("the record or the value stayed")
	}
	// The volume is emptied, not removed.
	if !maps.Equal(volumes, b.engine.Volumes) {
		t.Errorf("volumes changed: %v", b.engine.Volumes)
	}
	if last := b.engine.VolumeRuns[len(b.engine.VolumeRuns)-1]; !strings.HasSuffix(last.Job.Volume, "_github") || last.Input != nil {
		t.Errorf("the last volume job: %+v", last)
	}
}

func TestChangesWithoutDockerAreAppliedByUp(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.web.Accept("octo")
	b.dialErr = errors.New("no daemon")

	b.stdin = patNew
	code, out, stderr := b.run("token", "set", "web", "github", "read", "--no-open")
	if code != 0 || !strings.Contains(stderr, "saved, but not yet in the environment (no daemon) — bh up web applies it") {
		t.Fatalf("exit %d\n%s%s", code, out, stderr)
	}
	b.ok("var", "set", "web", "A", "1")
	b.stdin = "v"
	b.ok("secret", "set", "web", "B")

	b.dialErr = nil
	b.ok("up", "web")
	if writes := b.writes("github"); len(writes) != 1 || !strings.HasPrefix(writes[0], patNew+"\n") {
		t.Errorf("github volume: %q", writes)
	}
	if got := b.lastEnvWrite(); got != "A=1\nB=v\n" {
		t.Errorf("the environment got %q", got)
	}

	// Taken away without Docker: the next start clears the volumes, once.
	b.dialErr = errors.New("no daemon")
	b.web.Revocable()
	b.ok("token", "remove", "web", "github", "--yes")
	b.ok("var", "unset", "web", "A")
	b.ok("secret", "remove", "web", "B")
	b.dialErr = nil
	before := len(b.engine.VolumeRuns)
	b.ok("up", "web")
	if after := len(b.engine.VolumeRuns); after != before+2 || b.lastEnvWrite() != "" {
		t.Errorf("%d volume jobs at the start, environment %q", after-before, b.lastEnvWrite())
	}
	b.ok("up", "web")
	if len(b.engine.VolumeRuns) != before+2 {
		t.Error("the volumes were written again though nothing is pending")
	}
}

func TestUpStopsWhenAValueCannotBeHad(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.stdin = "v"
	b.ok("secret", "set", "web", "API_KEY")
	// The keychain no longer has it (locked, or cleared by hand).
	clear(b.keychain)
	if err := b.fails(1, "up", "web"); !strings.Contains(err, "the value of the secret API_KEY") {
		t.Errorf("up: %s", err)
	}
	if len(b.engine.Containers) != 0 {
		t.Error("the environment started without what its user counts on")
	}
}

func TestDestroyRevokesTokensAndForgetsValues(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.ok("up", "web")
	b.seedGitHub("web", patNew)
	b.seedHuggingFace("web", hfToken)
	b.stdin = "v"
	b.ok("secret", "set", "web", "API_KEY")
	b.web.Revocable()

	b.typing, b.stdin = true, "web\n"
	code, out, stderr := b.run("destroy", "web")
	if code != 0 || !strings.Contains(stderr, "Its tokens (github, huggingface) are revoked at their services") {
		t.Fatalf("exit %d\n%s%s", code, out, stderr)
	}
	if len(b.web.To("/credentials/revoke")) != 1 || len(b.web.To("/api/credentials/revoke")) != 1 {
		t.Errorf("revocations: %v", b.web.Requests())
	}
	// The keychain is left without a trace of the environment.
	if len(b.keychain) != 0 {
		t.Errorf("keychain after destroy: %v", b.keychain)
	}
}

func TestDestroyGoesOnWhenATokenCannotBeRevoked(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.seedGitHub("web", patNew)
	b.web.Reply("/credentials/revoke", http.StatusInternalServerError, "")
	code, out, stderr := b.run("destroy", "web", "--yes")
	if code != 0 || !strings.Contains(out, `Destroyed "web"`) ||
		!strings.Contains(stderr, "revoking the GitHub token failed (HTTP 500) — it still works; revoke \"bh-web-") {
		t.Fatalf("exit %d\n%s%s", code, out, stderr)
	}
	if len(b.keychain) != 0 {
		t.Errorf("keychain after destroy: %v", b.keychain)
	}
}

func TestRecordsOfEarlierBuildsAreTakenOver(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	id := b.env("web").ID
	dir := b.store().EnvDir(id)
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("github.json", `{"token": "`+patNew+`", "preset": "push+pr", "owner": "octo", "tokenName": "bh-web-10121530",
		"issuedAt": "2026-10-12T06:30:00.000Z", "expiresAt": "2027-10-12T06:30:00.000Z", "repos": ["octo/a"]}`)
	write("tokens.json", `{"tokens": [
		{"kind": "hf", "name": "HF_TOKEN", "value": "`+hfToken+`", "addedAt": "2026-10-13T00:00:00.000Z"},
		{"kind": "custom", "name": "API_KEY", "value": "k-123", "addedAt": "2026-10-14T00:00:00.000Z"}]}`)

	// Listing is enough to take them over: the values leave the files.
	list := b.ok("token", "list", "web")
	if !strings.Contains(list, "contribute · octo · 1 repo") || !strings.Contains(list, "huggingface") {
		t.Errorf("list:\n%s", list)
	}
	if b.kept(id, "token/github") != patNew || b.kept(id, "token/huggingface") != hfToken || b.kept(id, "secret/API_KEY") != "k-123" {
		t.Errorf("keychain = %v", b.keychain)
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		data, _ := os.ReadFile(filepath.Join(dir, entry.Name()))
		for _, value := range []string{patNew, hfToken, "k-123"} {
			if strings.Contains(string(data), value) {
				t.Errorf("%s still holds a value", entry.Name())
			}
		}
	}
	if list := b.ok("secret", "list", "web"); list != "API_KEY\n" {
		t.Errorf("secrets: %q", list)
	}
	b.ok("up", "web")
	if got := b.lastEnvWrite(); got != "API_KEY=k-123\nHF_TOKEN="+hfToken+"\n" {
		t.Errorf("the environment got %q", got)
	}
	if writes := b.writes("github"); len(writes) != 1 || !strings.HasPrefix(writes[0], patNew+"\npreset=contribute\n") {
		t.Errorf("github volume: %q", writes)
	}
}

// bh migrate takes the TypeScript version's token records over: the values
// go to where this machine keeps them, and the old preset name is read as
// today's.
func TestMigrateTakesTheTokenRecordsOver(t *testing.T) {
	b := newBench(t)
	dir := b.legacyEnv("web", `{"type": "local", "sshPort": 22001}`)
	b.write(filepath.Join(dir, "github.json"), `{"token": "`+patNew+`", "preset": "push+pr", "owner": "octo", "tokenName": "bh-web-10121530",
		"issuedAt": "2026-10-12T06:30:00.000Z", "expiresAt": "2027-10-12T06:30:00.000Z", "repos": ["octo/a", "octo/b"]}`)
	b.write(filepath.Join(dir, "tokens.json"), `{"tokens": [
		{"kind": "hf", "name": "HF_TOKEN", "value": "`+hfToken+`", "addedAt": "2026-10-13T00:00:00.000Z"},
		{"kind": "custom", "name": "API_KEY", "value": "k-123", "addedAt": "2026-10-14T00:00:00.000Z"}]}`)
	b.engine.AddLegacy("web", false)
	volumes := maps.Clone(b.engine.Volumes)

	out := b.ok("migrate", "web", "--yes")
	if strings.Contains(out, "not taken over yet") {
		t.Errorf("output:\n%s", out)
	}
	id := b.env("web").ID
	if b.kept(id, "token/github") != patNew || b.kept(id, "token/huggingface") != hfToken || b.kept(id, "secret/API_KEY") != "k-123" {
		t.Errorf("keychain = %v", b.keychain)
	}
	if github := b.githubState("web"); github == nil || github.Preset != "contribute" || len(github.Repos) != 2 {
		t.Errorf("github = %+v", github)
	}
	entries, _ := os.ReadDir(b.store().EnvDir(id))
	for _, entry := range entries {
		data, _ := os.ReadFile(filepath.Join(b.store().EnvDir(id), entry.Name()))
		for _, value := range []string{patNew, hfToken, "k-123"} {
			if strings.Contains(string(data), value) {
				t.Errorf("%s in bh's directory still holds a value", entry.Name())
			}
		}
	}
	// The old version's own files are where they were moved to, as they
	// were: handing the environment back needs them.
	b.noVolumeRemoved(volumes)
	b.ok("up", "web")
	if got := b.lastEnvWrite(); got != "API_KEY=k-123\nHF_TOKEN="+hfToken+"\n" {
		t.Errorf("the environment got %q", got)
	}
}

// Where there is no credential store and nothing is decided, the migration
// still goes through; the records wait in their old form.
func TestMigrateWithoutACredentialStoreLeavesTheRecordsForLater(t *testing.T) {
	b := newBench(t)
	b.noKeychain = true
	dir := b.legacyEnv("web", `{"type": "local", "sshPort": 22001}`)
	b.write(filepath.Join(dir, "tokens.json"), `{"tokens": [{"kind": "custom", "name": "API_KEY", "value": "k-123", "addedAt": "2026-10-14T00:00:00.000Z"}]}`)
	b.engine.AddLegacy("web", false)

	out := b.ok("migrate", "web", "--yes")
	if !strings.Contains(out, "its tokens were not taken over yet") || !strings.Contains(out, "bh secret store file") {
		t.Errorf("output:\n%s", out)
	}
	if err := b.fails(1, "up", "web"); !strings.Contains(err, "bh secret store file") {
		t.Errorf("up: %s", err)
	}
	b.ok("secret", "store", "file")
	b.ok("up", "web")
	if got := b.lastEnvWrite(); got != "API_KEY=k-123\n" {
		t.Errorf("the environment got %q", got)
	}
	if list := b.ok("secret", "list", "web"); list != "API_KEY\n" {
		t.Errorf("secrets: %q", list)
	}
}
