package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/github"
	"github.com/ys319/bulkhead/internal/github/githubtest"
)

const token = "github_pat_11AAAAAAA0aaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestTokenName(t *testing.T) {
	now := time.Date(2026, 10, 12, 15, 30, 0, 0, time.Local)
	if got := github.TokenName("main", now); got != "bh-main-10121530" {
		t.Errorf("TokenName = %s", got)
	}
	// A long environment name is cut; the time, which tells tokens apart, is not.
	long := github.TokenName(strings.Repeat("a", 32), now)
	if len(long) != 40 || !strings.HasSuffix(long, "-10121530") {
		t.Errorf("TokenName = %s (%d)", long, len(long))
	}
}

func TestEveryPresetHasPermissionsAndNoneHasWorkflows(t *testing.T) {
	for _, preset := range config.GitHubPresets {
		permissions, known := github.Permissions(preset)
		if !known || len(permissions) == 0 {
			t.Errorf("preset %s has no permissions", preset)
		}
		for _, permission := range permissions {
			if permission.Name == "workflows" {
				t.Errorf("preset %s grants workflows", preset)
			}
		}
	}
	if _, known := github.Permissions("admin"); known {
		t.Error("admin is a preset")
	}
}

func TestTemplateURL(t *testing.T) {
	parsed, err := url.Parse(github.TemplateURL(github.Template{
		Env: "main", Preset: "push+pr", Owner: "acme", Days: 30, TokenName: "bh-main-10121530", Hostname: "mac",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Scheme + "://" + parsed.Host + parsed.Path; got != "https://github.com/settings/personal-access-tokens/new" {
		t.Errorf("address = %s", got)
	}
	query := parsed.Query()
	for key, want := range map[string]string{
		"name": "bh-main-10121530", "target_name": "acme", "expires_in": "30",
		"contents": "write", "pull_requests": "write",
		"description": `bulkhead environment "main" on mac (preset: push+pr)`,
	} {
		if got := query.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if query.Has("workflows") {
		t.Error("workflows is filled in")
	}

	// read is contents:read alone; without an owner the page defaults to
	// the user's own account.
	query = mustQuery(t, github.TemplateURL(github.Template{Env: "main", Preset: "read", Days: 7, TokenName: "t", Hostname: "mac"}))
	if query.Get("contents") != "read" || query.Has("pull_requests") || query.Has("target_name") || query.Get("expires_in") != "7" {
		t.Errorf("query = %v", query)
	}
}

func mustQuery(t *testing.T, address string) url.Values {
	t.Helper()
	parsed, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Query()
}

func TestParseExpiration(t *testing.T) {
	tests := []struct{ header, want string }{
		{"2026-11-11 12:00:00 UTC", "2026-11-11T12:00:00Z"},
		{"2026-11-11 12:00:00 +0900", "2026-11-11T03:00:00Z"},
		{" 2026-11-11 12:00:00 -0130 ", "2026-11-11T13:30:00Z"},
		{"", ""},
		{"soon", ""},
		{"2026-11-11", ""},
		{"2026-11-11T12:00:00Z", ""},
	}
	for _, tt := range tests {
		parsed, ok := github.ParseExpiration(tt.header)
		got := ""
		if ok {
			got = parsed.Format(time.RFC3339)
		}
		if got != tt.want {
			t.Errorf("%q: %q, want %q", tt.header, got, tt.want)
		}
	}
}

func TestNextPage(t *testing.T) {
	tests := []struct{ link, want string }{
		{`<https://api.github.com/user/repos?page=2>; rel="next", <https://api.github.com/user/repos?page=3>; rel="last"`, "https://api.github.com/user/repos?page=2"},
		{`<https://api.github.com/user/repos?page=1>; rel="prev"`, ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := github.NextPage(tt.link); got != tt.want {
			t.Errorf("%q: %q, want %q", tt.link, got, tt.want)
		}
	}
}

// listing is /user/repos for a token with foo (private: listed means
// selected) and bar (public) selected. baz is public and not selected: a
// fine-grained token can read every public repository, so it is listed too.
const listing = `[
	{"full_name": "octo/foo", "private": true},
	{"full_name": "octo/bar", "private": false},
	{"full_name": "octo/baz", "private": false}
]`

const (
	probeBar = "/repos/octo/bar/collaborators/octo/permission"
	probeBaz = "/repos/octo/baz/collaborators/octo/permission"
)

func accepting(t *testing.T) *githubtest.Web {
	web := githubtest.New(t)
	web.Reply("/user", http.StatusOK, `{"login": "octo"}`)
	web.Reply("/user/repos", http.StatusOK, listing)
	web.Reply(probeBar, http.StatusOK, `{"permission": "write"}`)
	web.Reply(probeBaz, http.StatusForbidden, `{"message": "Resource not accessible by personal access token"}`)
	return web
}

func inspect(t *testing.T, web *githubtest.Web) (github.TokenInfo, error) {
	t.Helper()
	return github.Client{HTTP: &http.Client{Transport: web}}.Inspect(context.Background(), token)
}

func TestInspect(t *testing.T) {
	web := accepting(t)
	web.Reply("/user", http.StatusOK, `{"login": "octo"}`, "github-authentication-token-expiration", "2026-11-11 12:00:00 UTC")
	info, err := inspect(t, web)
	if err != nil {
		t.Fatal(err)
	}
	if info.Login != "octo" || info.ExpiresAt.Format(time.RFC3339) != "2026-11-11T12:00:00Z" || !slices.Equal(info.Repos, []string{"octo/foo", "octo/bar"}) {
		t.Errorf("info = %+v", info)
	}
	// The token travels in the Authorization header and nowhere else.
	for _, request := range web.Requests() {
		if request.Authorization != "Bearer "+token {
			t.Errorf("%s: Authorization = %q", request.URL, request.Authorization)
		}
		if strings.Contains(request.URL, token) || strings.Contains(request.Body, token) {
			t.Errorf("the token is in the request: %+v", request)
		}
	}
	// Private repositories are not probed; public ones are.
	var probed []string
	for _, request := range web.Requests() {
		if strings.HasSuffix(request.Path(), "/permission") {
			probed = append(probed, request.Path())
		}
	}
	slices.Sort(probed)
	if !slices.Equal(probed, []string{probeBar, probeBaz}) {
		t.Errorf("probed = %v", probed)
	}
}

func TestInspectTellsWhatItCannotTell(t *testing.T) {
	t.Run("a probe that fails otherwise than 403 or 404 leaves the repositories unknown", func(t *testing.T) {
		web := accepting(t)
		web.Reply(probeBar, http.StatusInternalServerError, "")
		if info, err := inspect(t, web); err != nil || info.Repos != nil {
			t.Errorf("info = %+v, %v", info, err)
		}
	})
	t.Run("a probe that cannot be made leaves them unknown", func(t *testing.T) {
		web := accepting(t)
		web.Fail(probeBar, errors.New("connection reset"))
		if info, err := inspect(t, web); err != nil || info.Repos != nil {
			t.Errorf("info = %+v, %v", info, err)
		}
	})
	t.Run("404 is not selected", func(t *testing.T) {
		web := accepting(t)
		web.Reply(probeBar, http.StatusNotFound, "")
		if info, err := inspect(t, web); err != nil || !slices.Equal(info.Repos, []string{"octo/foo"}) {
			t.Errorf("info = %+v, %v", info, err)
		}
	})
	t.Run("a listing that fails leaves them unknown, and the token still passes", func(t *testing.T) {
		web := accepting(t)
		web.Reply("/user/repos", http.StatusForbidden, "")
		info, err := inspect(t, web)
		if err != nil || info.Repos != nil || !info.ExpiresAt.IsZero() {
			t.Errorf("info = %+v, %v", info, err)
		}
	})
	t.Run("no repositories is known, and is none", func(t *testing.T) {
		web := accepting(t)
		web.Reply("/user/repos", http.StatusOK, "[]")
		if info, err := inspect(t, web); err != nil || info.Repos == nil || len(info.Repos) != 0 {
			t.Errorf("info = %+v, %v", info, err)
		}
	})
	t.Run("a name that is not a repository's leaves them unknown", func(t *testing.T) {
		web := accepting(t)
		web.Reply("/user/repos", http.StatusOK, `[{"full_name": "octo/x\nexpires_epoch=0", "private": true}]`)
		if info, err := inspect(t, web); err != nil || info.Repos != nil {
			t.Errorf("info = %+v, %v", info, err)
		}
	})
}

func TestInspectFollowsThePagesOfTheListing(t *testing.T) {
	web := accepting(t)
	pages := 0
	web.Handle("/user/repos", func(request *http.Request) (*http.Response, error) {
		pages++
		if request.URL.Query().Get("page") == "" {
			return githubtest.Response(http.StatusOK, `[{"full_name": "octo/one", "private": true}]`, "Link",
				`<https://api.github.com/user/repos?per_page=100&page=2>; rel="next", <https://api.github.com/user/repos?per_page=100&page=2>; rel="last"`), nil
		}
		return githubtest.Response(http.StatusOK, `[{"full_name": "octo/two", "private": true}]`), nil
	})
	info, err := inspect(t, web)
	if err != nil || !slices.Equal(info.Repos, []string{"octo/one", "octo/two"}) || pages != 2 {
		t.Errorf("info = %+v, %v after %d pages", info, err, pages)
	}

	// The next page is wherever a response says. The token does not follow
	// it off GitHub's API.
	web = accepting(t)
	web.Reply("/user/repos", http.StatusOK, `[]`, "Link", `<https://example.test/user/repos?page=2>; rel="next"`)
	info, err = inspect(t, web)
	if err != nil || info.Repos != nil {
		t.Errorf("info = %+v, %v", info, err)
	}
	for _, request := range web.Requests() {
		if !strings.HasPrefix(request.URL, "https://api.github.com/") {
			t.Errorf("a request went to %s", request.URL)
		}
	}

	// A listing without end is given up on.
	web = accepting(t)
	pages = 0
	web.Handle("/user/repos", func(*http.Request) (*http.Response, error) {
		pages++
		return githubtest.Response(http.StatusOK, `[]`, "Link", `<https://api.github.com/user/repos?page=next>; rel="next"`), nil
	})
	if info, err := inspect(t, web); err != nil || info.Repos != nil || pages != 10 {
		t.Errorf("info = %+v, %v after %d pages", info, err, pages)
	}
}

func TestInspectRefusesATokenGitHubDoesNotAccept(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*githubtest.Web)
		want  string
	}{
		{"401", func(web *githubtest.Web) { web.Reply("/user", http.StatusUnauthorized, "") }, "rejected the token"},
		{"403", func(web *githubtest.Web) {
			web.Reply("/user", http.StatusForbidden, "needs \x1b[31mapproval")
		}, "HTTP 403) — an organization may still need to approve it\nneeds [31mapproval"},
		{"no login", func(web *githubtest.Web) { web.Reply("/user", http.StatusOK, `{"login": 5}`) }, "unexpected response"},
		{"not JSON", func(web *githubtest.Web) { web.Reply("/user", http.StatusOK, `<html>`) }, "unexpected response"},
		{"no network", func(web *githubtest.Web) { web.Fail("/user", errors.New("no route to host")) }, "cannot reach api.github.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			web := githubtest.New(t)
			tt.setup(web)
			_, err := inspect(t, web)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
			if err != nil && strings.Contains(err.Error(), token) {
				t.Errorf("the error shows the token: %v", err)
			}
			if len(web.Requests()) != 1 {
				t.Errorf("requests = %+v", web.Requests())
			}
		})
	}
}

func TestRevoke(t *testing.T) {
	web := githubtest.New(t)
	web.Revocable()
	client := github.Client{HTTP: &http.Client{Transport: web}}
	if err := client.Revoke(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	request := web.Requests()[0]
	var body struct{ Credentials []string }
	if err := json.Unmarshal([]byte(request.Body), &body); err != nil {
		t.Fatal(err)
	}
	// The endpoint takes no authentication: the token is in the body alone.
	if request.Method != http.MethodPost || !slices.Equal(body.Credentials, []string{token}) || request.Authorization != "" {
		t.Errorf("request = %+v", request)
	}

	web.Reply("/credentials/revoke", http.StatusUnprocessableEntity, "nope\n")
	if err := client.Revoke(context.Background(), token); err == nil || err.Error() != "HTTP 422 nope" {
		t.Errorf("err = %v", err)
	}
	web.Reply("/credentials/revoke", http.StatusServiceUnavailable, "")
	if err := client.Revoke(context.Background(), token); err == nil || err.Error() != "HTTP 503" {
		t.Errorf("err = %v", err)
	}
	web.Fail("/credentials/revoke", errors.New("no route to host"))
	err := client.Revoke(context.Background(), token)
	if err == nil || !strings.Contains(err.Error(), "no route to host") || strings.Contains(err.Error(), token) {
		t.Errorf("err = %v", err)
	}
}
