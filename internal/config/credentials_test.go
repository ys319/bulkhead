package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNamesAndValues(t *testing.T) {
	for _, name := range []string{"API_KEY", "http_proxy", "_private", "Mixed9"} {
		if err := CheckName(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, part := range map[string]string{
		"":                      "letters, digits",
		"9LIVES":                "letters, digits",
		"WITH-DASH":             "letters, digits",
		"WITH SPACE":            "letters, digits",
		"GH_TOKEN":              "github token's",
		"GITHUB_TOKEN":          "github token's",
		"HF_TOKEN":              "huggingface token's",
		"PATH":                  "reserved",
		"LD_PRELOAD":            "reserved",
		"BH_ANYTHING":           "reserved",
		strings.Repeat("A", 65): "letters, digits",
	} {
		if err := CheckName(name); err == nil || !strings.Contains(err.Error(), part) {
			t.Errorf("%q: %v", name, err)
		}
	}
	for _, value := range []string{"plain", "with spaces", "it's \"quoted\"", "日本語 の値", "a=b&c=d", "$(not run)"} {
		if err := CheckValue(value); err != nil {
			t.Errorf("%q: %v", value, err)
		}
	}
	for _, value := range []string{"", "two\nlines", "tab\there", "bell\a", "bad\xffutf8", strings.Repeat("x", 16385)} {
		if err := CheckValue(value); err == nil {
			t.Errorf("%q passed", value)
		}
	}
}

func TestTokensAndSecretsAreKeptWithoutValues(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	env := NewLocal("e1", "web", 22001)
	if err := store.SaveEnvironment(env); err != nil {
		t.Fatal(err)
	}
	if tokens, err := store.LoadTokens("e1"); err != nil || tokens != nil {
		t.Fatalf("no file: %v %v", tokens, err)
	}

	expiry := Timestamp(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	tokens := []Token{
		{Service: ServiceGitHub, Preset: "push", Owner: "octo", TokenName: "bh-web-1", IssuedAt: "2026-11-01T00:00:00.000Z", ExpiresAt: &expiry, Repos: []string{"octo/a"}},
		{Service: ServiceHuggingFace, Access: "read", Owner: "me", IssuedAt: "2026-11-01T00:00:00.000Z"},
	}
	if err := store.SaveTokens("e1", tokens); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSecrets("e1", []Secret{{Name: "API_KEY", AddedAt: "2026-11-01T00:00:00.000Z"}}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{store.TokensFile("e1"), store.SecretsFile("e1")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", path, info, err)
		}
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), "value") {
			t.Errorf("%s has a place for values:\n%s", path, data)
		}
	}
	got, err := store.LoadTokens("e1")
	if err != nil || len(got) != 2 || got[0].Preset != "push" || got[1].Access != "read" || !slices.Equal(got[0].Repos, []string{"octo/a"}) {
		t.Errorf("tokens = %+v, %v", got, err)
	}
	if left, expires := got[0].DaysLeft(time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)); !expires || left != 4 || got[0].Urgency(time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)) != "soon" {
		t.Errorf("days left = %d %v", left, expires)
	}
	if _, expires := got[1].DaysLeft(time.Now()); expires {
		t.Error("a token without an expiry expires")
	}
	secrets, err := store.LoadSecrets("e1")
	if err != nil || len(secrets) != 1 || secrets[0].Name != "API_KEY" {
		t.Errorf("secrets = %+v, %v", secrets, err)
	}

	// With the last one gone, so is the file.
	if err := store.SaveTokens("e1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.TokensFile("e1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the empty tokens file stayed: %v", err)
	}

	// A file that cannot be read is an error, not "no tokens".
	if err := os.WriteFile(store.TokensFile("e1"), []byte(`{"version": 1, "tokens": [{"service": "github", "typo": 1}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadTokens("e1"); err == nil {
		t.Error("a file with an unknown field was read")
	}
}

func TestLegacyRecordsAreTakenOver(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	if err := store.SaveEnvironment(NewLocal("e1", "web", 22001)); err != nil {
		t.Fatal(err)
	}
	if legacy, err := store.HasLegacyCredentials("e1"); err != nil || legacy {
		t.Fatalf("nothing there: %v %v", legacy, err)
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(store.EnvDir("e1"), name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("github.json", `{"token": "github_pat_OLD", "preset": "push+pr", "owner": "octo", "tokenName": "bh-web-10121530",
		"issuedAt": "2026-10-12T06:30:00.000Z", "expiresAt": "2026-11-11T06:30:00.000Z", "repos": ["octo/a", "octo/b"]}`)
	write("tokens.json", `{"tokens": [
		{"kind": "hf", "name": "HF_TOKEN", "value": "hf_abc", "addedAt": "2026-10-13T00:00:00.000Z"},
		{"kind": "custom", "name": "API_KEY", "value": "k-123", "addedAt": "2026-10-14T00:00:00.000Z"}]}`)
	if legacy, err := store.HasLegacyCredentials("e1"); err != nil || !legacy {
		t.Fatalf("legacy files: %v %v", legacy, err)
	}
	if _, err := store.LoadTokens("e1"); err == nil {
		t.Error("the unversioned tokens.json was read as today's")
	}

	values := map[string]string{}
	put := func(key, value string) error { values[key] = value; return nil }

	// The vault fails: nothing is rewritten, and a second run does it all.
	if err := store.ConvertLegacyCredentials("e1", func(string, string) error { return errors.New("locked") }); err == nil {
		t.Fatal("converted without a vault")
	}
	if legacy, _ := store.HasLegacyCredentials("e1"); !legacy {
		t.Fatal("the legacy files went although the values were not kept")
	}

	if err := store.ConvertLegacyCredentials("e1", put); err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"token/github": "github_pat_OLD", "token/huggingface": "hf_abc", "secret/API_KEY": "k-123"}; len(values) != 3 ||
		values["token/github"] != want["token/github"] || values["token/huggingface"] != want["token/huggingface"] || values["secret/API_KEY"] != want["secret/API_KEY"] {
		t.Errorf("values = %v", values)
	}
	tokens, err := store.LoadTokens("e1")
	if err != nil || len(tokens) != 2 {
		t.Fatalf("tokens = %+v, %v", tokens, err)
	}
	github := tokens[slices.IndexFunc(tokens, func(t Token) bool { return t.Service == ServiceGitHub })]
	if github.Preset != "contribute" || github.Owner != "octo" || github.TokenName != "bh-web-10121530" || len(github.Repos) != 2 || github.ExpiresAt == nil {
		t.Errorf("github = %+v", github)
	}
	secrets, err := store.LoadSecrets("e1")
	if err != nil || len(secrets) != 1 || secrets[0].Name != "API_KEY" {
		t.Errorf("secrets = %+v, %v", secrets, err)
	}
	if _, err := os.Stat(filepath.Join(store.EnvDir("e1"), "github.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("github.json stayed: %v", err)
	}
	for _, name := range []string{"tokens.json", "secrets.json"} {
		data, _ := os.ReadFile(filepath.Join(store.EnvDir("e1"), name))
		for _, value := range []string{"github_pat_OLD", "hf_abc", "k-123"} {
			if strings.Contains(string(data), value) {
				t.Errorf("%s still holds %s", name, value)
			}
		}
	}
	if legacy, _ := store.HasLegacyCredentials("e1"); legacy {
		t.Error("still legacy after the conversion")
	}
}

func TestVarsAreSettings(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	if _, err := store.Init(); err != nil {
		t.Fatal(err)
	}
	env := NewLocal("e1", "web", 22001)
	env.Vars = map[string]string{"EDITOR_THEME": "dark mode", "http_proxy": "http://proxy:8080"}
	if err := store.SaveEnvironment(env); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.Load()
	if err != nil || len(cfg.Envs) != 1 || cfg.Envs[0].Vars["EDITOR_THEME"] != "dark mode" || len(cfg.Envs[0].Vars) != 2 {
		t.Fatalf("loaded: %+v, %v", cfg, err)
	}
	for name, value := range map[string]string{"GH_TOKEN": "x", "PATH": "/bin", "OK": "two\nlines"} {
		bad := NewLocal("e2", "api", 22002)
		bad.Vars = map[string]string{name: value}
		if err := store.SaveEnvironment(bad); err == nil {
			t.Errorf("%s=%q was saved", name, value)
		}
	}
	// A remote environment has none: its variables are set where it runs.
	data, _ := os.ReadFile(store.EnvFile("e1"))
	if !strings.Contains(string(data), `"vars"`) {
		t.Errorf("env.json:\n%s", data)
	}
}
