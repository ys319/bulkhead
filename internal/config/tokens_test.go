package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	testToken = "github_pat_11AAAAAAA0aaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	// What the TypeScript version writes for sampleGitHub: JSON.stringify
	// with two spaces and a final line break.
	githubGolden = `{
  "token": "` + testToken + `",
  "preset": "push",
  "owner": "octo",
  "tokenName": "bh-main-10121530",
  "issuedAt": "2026-10-12T06:30:00.000Z",
  "expiresAt": "2026-11-11T06:30:00.000Z",
  "repos": [
    "octo/foo",
    "octo/bar"
  ]
}
`
)

func sampleGitHub() GitHubState {
	expires := "2026-11-11T06:30:00.000Z"
	return GitHubState{
		Token: testToken, Preset: "push", Owner: "octo", TokenName: "bh-main-10121530",
		IssuedAt: "2026-10-12T06:30:00.000Z", ExpiresAt: &expires, Repos: []string{"octo/foo", "octo/bar"},
	}
}

// envStore is a store with the directory of one environment.
func envStore(t *testing.T) (Store, string) {
	t.Helper()
	store := Store{Dir: t.TempDir()}
	if err := os.MkdirAll(store.EnvDir("main"), 0o755); err != nil {
		t.Fatal(err)
	}
	return store, "main"
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestGitHubStateIsTheFileOfTheTypeScriptVersion(t *testing.T) {
	store, id := envStore(t)
	if state, err := store.LoadGitHub(id); state != nil || err != nil {
		t.Fatalf("no file: %v, %v", state, err)
	}
	if err := store.SaveGitHub(id, sampleGitHub()); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(store.GitHubFile(id))
	if string(data) != githubGolden {
		t.Errorf("github.json:\n%s\nwant:\n%s", data, githubGolden)
	}
	if runtime.GOOS != "windows" && mode(t, store.GitHubFile(id)) != 0o600 {
		t.Errorf("mode = %o", mode(t, store.GitHubFile(id)))
	}

	// And back, from the bytes the TypeScript version would have written.
	if err := os.WriteFile(store.GitHubFile(id), []byte(githubGolden), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := store.LoadGitHub(id)
	if err != nil {
		t.Fatal(err)
	}
	if state.Token != testToken || state.Owner != "octo" || len(state.Repos) != 2 || *state.ExpiresAt != "2026-11-11T06:30:00.000Z" {
		t.Errorf("state = %+v", state)
	}

	// Unknown repositories and no expiry are null, not left out; no
	// repositories is an empty list, not null.
	unknown := sampleGitHub()
	unknown.Repos, unknown.ExpiresAt = nil, nil
	if err := store.SaveGitHub(id, unknown); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(store.GitHubFile(id))
	if !strings.Contains(string(data), `"expiresAt": null,`+"\n"+`  "repos": null`+"\n") {
		t.Errorf("github.json:\n%s", data)
	}
	if state, err := store.LoadGitHub(id); err != nil || state.Repos != nil || state.ExpiresAt != nil {
		t.Errorf("state = %+v, %v", state, err)
	}
	none := sampleGitHub()
	none.Repos = []string{}
	if err := store.SaveGitHub(id, none); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(store.GitHubFile(id))
	if !strings.Contains(string(data), `"repos": []`+"\n") {
		t.Errorf("github.json:\n%s", data)
	}
	if state, err := store.LoadGitHub(id); err != nil || state.Repos == nil || len(state.Repos) != 0 {
		t.Errorf("state = %+v, %v", state, err)
	}

	if err := store.RemoveGitHub(id); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveGitHub(id); err != nil {
		t.Errorf("removing what is not there: %v", err)
	}
}

func TestSecretsAreNeverWrittenReadableByOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no file modes")
	}
	store, id := envStore(t)
	// A temporary file left by an interrupted run, and a record, both too
	// open: the mode of a file only applies when it is created.
	for _, path := range []string{store.GitHubFile(id) + ".tmp", store.GitHubFile(id)} {
		if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveGitHub(id, sampleGitHub()); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, store.GitHubFile(id)); got != 0o600 {
		t.Errorf("mode = %o", got)
	}
	if _, err := os.Stat(store.GitHubFile(id) + ".tmp"); err == nil {
		t.Error("the temporary file was left behind")
	}
}

func TestLoadGitHubRefusesWhatIsNotARecord(t *testing.T) {
	store, id := envStore(t)
	edit := func(old, replacement string) string { return strings.Replace(githubGolden, old, replacement, 1) }
	tests := []struct{ name, content, want string }{
		{"not JSON", "{oops", "fix or delete"},
		{"an unknown field", edit(`"preset"`, `"extra": 1, "preset"`), "unknown field"},
		{"a missing field", edit(`  "expiresAt": "2026-11-11T06:30:00.000Z",`+"\n", ""), `"expiresAt" is missing`},
		{"a classic token", edit(testToken, "ghp_classic"), "fine-grained"},
		{"a token with a line break", edit(testToken, `github_pat_a\nb`), "fine-grained"},
		{"an unknown preset", edit(`"push"`, `"admin"`), "preset"},
		{"no owner", edit(`"octo"`, `""`), "owner"},
		{"an expiry that is not a time", edit("2026-11-11T06:30:00.000Z", "soon"), "not a time"},
		{"a control character", edit("octo/foo", `octo/\u001b[31m`), "control characters"},
		{"content after the value", githubGolden + "{}", "fix or delete"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(store.GitHubFile(id), []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			state, err := store.LoadGitHub(id)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
			if state != nil {
				t.Errorf("state = %+v", state)
			}
			if err != nil && strings.Contains(err.Error(), testToken) {
				t.Errorf("the error shows the token: %v", err)
			}
		})
	}
}

func TestGitHubExpiry(t *testing.T) {
	at := func(text string) time.Time {
		parsed, err := time.Parse(time.RFC3339, text)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	state := func(expires string) GitHubState {
		if expires == "" {
			return GitHubState{}
		}
		return GitHubState{ExpiresAt: &expires}
	}
	now := at("2026-11-01T00:00:00Z")
	tests := []struct {
		expires string
		left    int
		expiry  bool
		urgency string
	}{
		{"2026-10-31T00:00:00.000Z", -1, true, "expired"},
		{"2026-10-31T23:59:59.000Z", -1, true, "expired"}, // rounded down, also below zero
		{"2026-11-01T12:00:00.000Z", 0, true, "soon"},
		{"2026-11-05T00:00:00.000Z", 4, true, "soon"},
		{"2026-11-08T00:00:00.000Z", 7, true, "soon"},
		{"2026-12-01T00:00:00.000Z", 30, true, ""},
		{"", 0, false, ""},
	}
	for _, tt := range tests {
		left, expires := state(tt.expires).DaysLeft(now)
		if left != tt.left || expires != tt.expiry {
			t.Errorf("%q: DaysLeft = %d, %v; want %d, %v", tt.expires, left, expires, tt.left, tt.expiry)
		}
		if urgency := state(tt.expires).Urgency(now); urgency != tt.urgency {
			t.Errorf("%q: Urgency = %q, want %q", tt.expires, urgency, tt.urgency)
		}
	}
	if got := Timestamp(at("2026-10-12T15:30:00+09:00")); got != "2026-10-12T06:30:00.000Z" {
		t.Errorf("Timestamp = %s", got)
	}
}

func TestGitHubClearPendingMark(t *testing.T) {
	store, id := envStore(t)
	if store.GitHubClearPending(id) {
		t.Error("set from the start")
	}
	if err := store.SetGitHubClearPending(id, false); err != nil {
		t.Errorf("removing a mark that is not there: %v", err)
	}
	if err := store.SetGitHubClearPending(id, true); err != nil || !store.GitHubClearPending(id) {
		t.Errorf("not set: %v", err)
	}
	// The TypeScript version's name: a mark it left is found.
	if _, err := os.Stat(filepath.Join(store.EnvDir(id), "github.clear-pending")); err != nil {
		t.Error(err)
	}
	if err := store.SetGitHubClearPending(id, false); err != nil || store.GitHubClearPending(id) {
		t.Errorf("not removed: %v", err)
	}
}

func TestCheckTokenName(t *testing.T) {
	tests := []struct{ kind, name, want string }{
		{"custom", "OPENAI_API_KEY", ""},
		{"custom", "A1_B", ""},
		// Ways to shoot oneself in the foot are the user's to take.
		{"custom", "NODE_OPTIONS", ""},
		{"custom", "GIT_SSH_COMMAND", ""},
		{"custom", "lower", "A-Z"},
		{"custom", "1ST", "A-Z"},
		{"custom", "_X", "A-Z"},
		{"custom", "A-B", "A-Z"},
		{"custom", "A=B", "A-Z"},
		{"custom", "A\nB", "A-Z"},
		{"custom", "", "A-Z"},
		{"custom", strings.Repeat("A", 40), ""},
		{"custom", strings.Repeat("A", 41), "A-Z"},
		{"custom", "PATH", "reserved"},
		{"custom", "HOME", "reserved"},
		{"custom", "UID", "reserved"},
		{"custom", "PPID", "reserved"},
		{"custom", "PS1", "reserved"},
		{"custom", "IFS", "reserved"},
		{"custom", "ZDOTDIR", "reserved"},
		{"custom", "LD_PRELOAD", "reserved"},
		{"custom", "LC_ALL", "reserved"},
		{"custom", "SSH_AUTH_SOCK", "reserved"},
		{"custom", "NIX_PATH", "reserved"},
		{"custom", "ZSH_X", "reserved"},
		{"custom", "BULKHEAD_X", "reserved"},
		{"custom", "BH_X", "reserved"},
		{"custom", "GH_TOKEN", "bh github"},
		{"custom", "GITHUB_TOKEN", "bh github"},
		{"custom", "HF_TOKEN", "Hugging Face"},
		{"hf", "HF_TOKEN", ""},
		{"hf", "HUGGINGFACE_TOKEN", "HF_TOKEN"},
		{"github", "X", "kind"},
	}
	for _, tt := range tests {
		err := CheckTokenName(tt.kind, tt.name)
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("%s %q was refused: %v", tt.kind, tt.name, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("%s %q: %v, want it to mention %q", tt.kind, tt.name, err, tt.want)
		}
	}
}

func TestCheckTokenValue(t *testing.T) {
	if err := CheckTokenValue(`hf_abc-._~!#$%&()*+,/:;<=>?@[\]^` + "`" + `{|}"`); err != nil {
		t.Errorf("every printable character but the single quote: %v", err)
	}
	if err := CheckTokenValue(strings.Repeat("a", 4096)); err != nil {
		t.Errorf("4096 characters: %v", err)
	}
	for _, bad := range []string{"", "a b", "a'b", "a\nb", "a\n", "a\tb", "a\x7fb", "日本", strings.Repeat("a", 4097)} {
		if err := CheckTokenValue(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	if err := CheckTokenValue(strings.Repeat("a", 4097)); err == nil || !strings.Contains(err.Error(), "longer") {
		t.Errorf("too long: %v", err)
	}
}

func TestTokensFileIsTheFileOfTheTypeScriptVersion(t *testing.T) {
	store, id := envStore(t)
	if tokens, exists, err := store.LoadTokens(id); tokens != nil || exists || err != nil {
		t.Fatalf("no file: %v, %v, %v", tokens, exists, err)
	}
	tokens := []TokenEntry{
		{Kind: "hf", Name: "HF_TOKEN", Value: "hf_one", AddedAt: "2026-10-12T06:30:00.000Z"},
		// Characters Go's encoder would escape, and JSON.stringify does not.
		{Kind: "custom", Name: "K", Value: `a<b>&c="d"`, AddedAt: "2026-10-12T06:30:00.000Z"},
	}
	if err := store.SaveTokens(id, tokens); err != nil {
		t.Fatal(err)
	}
	const golden = `{
  "tokens": [
    {
      "kind": "hf",
      "name": "HF_TOKEN",
      "value": "hf_one",
      "addedAt": "2026-10-12T06:30:00.000Z"
    },
    {
      "kind": "custom",
      "name": "K",
      "value": "a<b>&c=\"d\"",
      "addedAt": "2026-10-12T06:30:00.000Z"
    }
  ]
}
`
	data, _ := os.ReadFile(store.TokensFile(id))
	if string(data) != golden {
		t.Errorf("tokens.json:\n%s\nwant:\n%s", data, golden)
	}
	if runtime.GOOS != "windows" && mode(t, store.TokensFile(id)) != 0o600 {
		t.Errorf("mode = %o", mode(t, store.TokensFile(id)))
	}
	loaded, exists, err := store.LoadTokens(id)
	if err != nil || !exists || len(loaded) != 2 || loaded[1] != tokens[1] {
		t.Errorf("loaded = %+v, %v, %v", loaded, exists, err)
	}

	// The last token gone leaves an empty list, not no file.
	if err := store.SaveTokens(id, nil); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(store.TokensFile(id))
	if string(data) != "{\n  \"tokens\": []\n}\n" {
		t.Errorf("tokens.json = %q", data)
	}
	if loaded, exists, err := store.LoadTokens(id); err != nil || !exists || len(loaded) != 0 {
		t.Errorf("loaded = %+v, %v, %v", loaded, exists, err)
	}
}

func TestLoadTokensRefusesWhatIsNotARecord(t *testing.T) {
	store, id := envStore(t)
	entry := func(kind, name, value string) string {
		return `{"tokens": [{"kind": "` + kind + `", "name": "` + name + `", "value": "` + value + `", "addedAt": "t"}]}`
	}
	tests := []struct{ name, content, want string }{
		{"not JSON", "{", "fix or delete"},
		{"another shape", `{"keys": []}`, `"tokens" is missing`},
		{"an unknown field", `{"tokens": [], "extra": 1}`, "unknown field"},
		{"an unknown kind", entry("github", "K", "v"), "kind"},
		{"a name that is more than a name", entry("custom", `K=1\nX`, "v"), "variable name"},
		// Would become a second line in the environment.
		{"a value with a line break", entry("custom", "K", `v\nPATH=/x`), "characters"},
		{"a value with a quote", entry("custom", "K", "a'b"), "characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(store.TokensFile(id), []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.LoadTokens(id); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
	// A name reserved since the entry was made still loads: the file is
	// read with the rules that protect the environment, not those for new
	// tokens.
	if err := os.WriteFile(store.TokensFile(id), []byte(entry("custom", "PATH", "v")), 0o600); err != nil {
		t.Fatal(err)
	}
	if tokens, _, err := store.LoadTokens(id); err != nil || len(tokens) != 1 {
		t.Errorf("tokens = %+v, %v", tokens, err)
	}
}
