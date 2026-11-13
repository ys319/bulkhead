// Package github is what bh needs from GitHub for an environment's token: a
// link to the page that issues one, a check of the token it gets back, and
// its revocation.
//
// GitHub has no API to issue a fine-grained personal access token; the user
// does that on GitHub's page, which bh opens filled in. The token is not a
// protection: everything inside the environment can read it. What limits
// the damage is the token's own scope, which is why the page is filled in
// with the least that the chosen preset needs.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// SettingsURL is where a user sees, issues and revokes tokens by hand.
	SettingsURL = "https://github.com/settings/personal-access-tokens"
	apiURL      = "https://api.github.com"

	// DefaultDays is how long a token lasts unless told otherwise; MaxDays
	// is the most the page takes.
	DefaultDays = 30
	MaxDays     = 366

	// maxTokenName is the longest name the page takes.
	maxTokenName = 40
)

// Permission is one permission of a token: what on, and how far.
type Permission struct{ Name, Access string }

// presets are the permissions the page is filled in with. "Metadata: read"
// is added by GitHub itself. Workflows is in none of them: whoever needs it
// adds it on the page, as their own decision.
//
// Reading the results of CI (actions, statuses) comes with push: whoever
// can push can already change what the workflows run, which reaches further
// than reading their logs does. It is not in read, which is often given for
// other repositories — dependencies — whose CI is none of the
// environment's business. Pull requests are the last step: with them the
// token can review in the user's name.
var presets = map[string][]Permission{
	"read":       {{"contents", "read"}},
	"push":       {{"contents", "write"}, {"actions", "read"}, {"statuses", "read"}},
	"contribute": {{"contents", "write"}, {"actions", "read"}, {"statuses", "read"}, {"pull_requests", "write"}},
}

// Permissions are the permissions of a preset; false for an unknown one.
func Permissions(preset string) ([]Permission, bool) {
	permissions, known := presets[preset]
	return permissions, known
}

// TokenName is the name a new token gets: bh-<env>-<MMDDhhmm>. The time
// tells the new token from the one it replaces while both exist. An
// environment name too long for the page's limit is cut, the time is not.
func TokenName(env string, now time.Time) string {
	stamp := now.Format("01021504")
	room := maxTokenName - len("bh-") - len("-") - len(stamp)
	if len(env) > room {
		env = env[:room]
	}
	return "bh-" + env + "-" + stamp
}

// Template is what the token page is filled in with.
type Template struct {
	Env       string // the environment's name, for the description
	Preset    string
	Owner     string // a user or organization; empty for the user's own account
	Days      int
	TokenName string
	Hostname  string // this machine, for the description
}

// TemplateURL is the address of the page that issues a token, filled in.
// The one thing that cannot be filled in is the choice of repositories.
func TemplateURL(t Template) string {
	pairs := [][2]string{
		{"name", t.TokenName},
		{"description", fmt.Sprintf("bulkhead environment %q on %s (preset: %s)", t.Env, t.Hostname, t.Preset)},
	}
	if t.Owner != "" {
		pairs = append(pairs, [2]string{"target_name", t.Owner})
	}
	pairs = append(pairs, [2]string{"expires_in", strconv.Itoa(t.Days)})
	for _, permission := range presets[t.Preset] {
		pairs = append(pairs, [2]string{permission.Name, permission.Access})
	}
	query := make([]string, len(pairs))
	for i, pair := range pairs {
		query[i] = url.QueryEscape(pair[0]) + "=" + url.QueryEscape(pair[1])
	}
	return SettingsURL + "/new?" + strings.Join(query, "&")
}

// ManualRevokeHint tells where to revoke a token that bh could not revoke.
func ManualRevokeHint(tokenName string) string {
	return fmt.Sprintf("revoke %q by hand at %s", tokenName, SettingsURL)
}

// ParseExpiration reads the response header that says when the token
// expires (for example "2026-11-11 12:00:00 UTC"). false for anything else.
func ParseExpiration(header string) (time.Time, bool) {
	header = strings.TrimSpace(header)
	for _, layout := range []string{"2006-01-02 15:04:05 UTC", "2006-01-02 15:04:05 -0700"} {
		if parsed, err := time.Parse(layout, header); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

var nextLink = regexp.MustCompile(`^<([^>]+)>\s*;\s*rel="next"$`)

// NextPage takes the rel="next" address out of a Link header; "" if none.
func NextPage(link string) string {
	for _, part := range strings.Split(link, ",") {
		if matched := nextLink.FindStringSubmatch(strings.TrimSpace(part)); matched != nil {
			return matched[1]
		}
	}
	return ""
}

// TokenInfo is what GitHub says about a token.
type TokenInfo struct {
	Login string
	// ExpiresAt is zero when GitHub did not say.
	ExpiresAt time.Time
	// Repos are the repositories the token's permissions apply to
	// (owner/name); nil when that could not be told.
	Repos []string
}

// Client talks to the GitHub API.
type Client struct {
	HTTP *http.Client
}

const (
	// maxRepoPages bounds the listing (100 repositories a page); with more,
	// the repositories count as unknown.
	maxRepoPages = 10
	// probeWorkers is how many public repositories are probed at a time.
	probeWorkers = 8
	// Bounds on what is read of a response.
	maxBody   = 16 << 20
	maxDetail = 500
)

func (c Client) do(ctx context.Context, method, address, token string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, address, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "bulkhead")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	// The token travels in this header only: never in the address, which
	// ends up in error messages and proxy logs.
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	return client.Do(request)
}

func ok(response *http.Response) bool { return response.StatusCode >= 200 && response.StatusCode < 300 }

// discard ends a response whose body is of no interest.
func discard(response *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxBody))
	response.Body.Close()
}

// detail is the start of a response's body, for an error message, without
// anything that would act on the terminal.
func detail(response *http.Response) string {
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, maxDetail))
	text := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return -1
		}
		return r
	}, string(data))
	return strings.TrimSpace(text)
}

// Inspect checks a token with GitHub: whose it is, when it expires and
// which repositories it reaches. A token GitHub does not accept is an
// error. Repositories that cannot be told are not: Repos is nil then.
func (c Client) Inspect(ctx context.Context, token string) (TokenInfo, error) {
	response, err := c.do(ctx, http.MethodGet, apiURL+"/user", token, nil)
	if err != nil {
		return TokenInfo{}, fmt.Errorf("cannot reach api.github.com to verify the token: %w", err)
	}
	if response.StatusCode == http.StatusUnauthorized {
		discard(response)
		return TokenInfo{}, errors.New("GitHub rejected the token (mistyped, revoked, or expired)")
	}
	if !ok(response) {
		message := fmt.Sprintf("verifying the token failed (HTTP %d) — an organization may still need to approve it", response.StatusCode)
		if text := detail(response); text != "" {
			message += "\n" + text
		}
		return TokenInfo{}, errors.New(message)
	}
	var user struct {
		Login string `json:"login"`
	}
	err = json.NewDecoder(io.LimitReader(response.Body, maxBody)).Decode(&user)
	response.Body.Close()
	if err != nil || user.Login == "" {
		return TokenInfo{}, errors.New("unexpected response from GitHub /user")
	}
	info := TokenInfo{Login: user.Login}
	info.ExpiresAt, _ = ParseExpiration(response.Header.Get("github-authentication-token-expiration"))

	if listed, known := c.listRepos(ctx, token); known {
		info.Repos = c.grantedRepos(ctx, token, user.Login, listed)
	}
	return info, nil
}

type listedRepo struct {
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
}

// What a repository's full name looks like. It goes into an address and,
// later, into a line of the environment's metadata.
var repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// listRepos reads every page of /user/repos; false when it cannot.
func (c Client) listRepos(ctx context.Context, token string) ([]listedRepo, bool) {
	listed := []listedRepo{}
	address := apiURL + "/user/repos?per_page=100"
	for page := 0; address != ""; page++ {
		// The token goes wherever this address points, and after the first
		// page the address comes out of a response.
		if page >= maxRepoPages || !strings.HasPrefix(address, apiURL+"/") {
			return nil, false
		}
		response, err := c.do(ctx, http.MethodGet, address, token, nil)
		if err != nil {
			return nil, false
		}
		if !ok(response) {
			discard(response)
			return nil, false
		}
		var items []listedRepo
		err = json.NewDecoder(io.LimitReader(response.Body, maxBody)).Decode(&items)
		response.Body.Close()
		if err != nil {
			return nil, false
		}
		for _, item := range items {
			if !repoName.MatchString(item.FullName) {
				return nil, false
			}
			listed = append(listed, item)
		}
		address = NextPage(response.Header.Get("Link"))
	}
	return listed, true
}

// grantedRepos keeps the listed repositories the token's permissions apply
// to. A fine-grained token can always read public repositories, so the
// listing includes public ones that were not selected for it; and the
// permissions the listing reports are the user's, not the token's. A
// private repository that is listed was selected. A public one is told
// apart by an endpoint that needs "Metadata: read" on that repository:
// selected answers 200, not selected 403 (or 404). Any other answer means
// it cannot be told, and then none of it is claimed: nil.
func (c Client) grantedRepos(ctx context.Context, token, login string, listed []listedRepo) []string {
	const (
		granted = iota + 1
		notGranted
	)
	verdicts := make([]int, len(listed))
	next := make(chan int)
	var workers sync.WaitGroup
	for range min(probeWorkers, len(listed)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range next {
				if listed[i].Private {
					verdicts[i] = granted
					continue
				}
				address := apiURL + "/repos/" + listed[i].FullName + "/collaborators/" + url.PathEscape(login) + "/permission"
				response, err := c.do(ctx, http.MethodGet, address, token, nil)
				if err != nil {
					continue
				}
				switch {
				case ok(response):
					verdicts[i] = granted
				case response.StatusCode == http.StatusForbidden, response.StatusCode == http.StatusNotFound:
					verdicts[i] = notGranted
				}
				discard(response)
			}
		}()
	}
	for i := range listed {
		next <- i
	}
	close(next)
	workers.Wait()

	repos := []string{}
	for i, verdict := range verdicts {
		switch verdict {
		case granted:
			repos = append(repos, listed[i].FullName)
		case notGranted:
		default:
			return nil
		}
	}
	return repos
}

// Revoke makes GitHub revoke the token. The endpoint takes no
// authentication (the token is its own proof) and notifies the owner.
func (c Client) Revoke(ctx context.Context, token string) error {
	body, err := json.Marshal(map[string][]string{"credentials": {token}})
	if err != nil {
		return err
	}
	response, err := c.do(ctx, http.MethodPost, apiURL+"/credentials/revoke", "", body)
	if err != nil {
		return err
	}
	if ok(response) {
		discard(response)
		return nil
	}
	return errors.New(strings.TrimSpace(fmt.Sprintf("HTTP %d %s", response.StatusCode, detail(response))))
}
