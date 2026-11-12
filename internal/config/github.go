package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// GitHubPresets are the permission sets a GitHub token can be issued with,
// by name. There is no default among them: the choice is made every time.
var GitHubPresets = []string{"read", "push", "push+pr"}

// A fine-grained personal access token. bh-guest accepts the same
// characters and nothing else.
var githubTokenPattern = regexp.MustCompile(`^github_pat_[A-Za-z0-9_]+$`)

// ValidGitHubToken reports whether text has the form of a fine-grained
// personal access token. Whether GitHub accepts it is another question.
func ValidGitHubToken(text string) bool { return githubTokenPattern.MatchString(text) }

// GitHubState is the environment's GitHub token as kept on this machine
// (github.json, mode 0600). The copy is kept to be able to revoke the token
// when it is replaced or the environment is destroyed.
//
// The file is the one the TypeScript version wrote, field for field and in
// this order: a migrated environment brings it along as it is.
type GitHubState struct {
	Token  string `json:"token"`
	Preset string `json:"preset"`
	// Owner is the user or organization the token belongs to.
	Owner string `json:"owner"`
	// TokenName is the name the token was given on GitHub's page: what to
	// look for there.
	TokenName string `json:"tokenName"`
	IssuedAt  string `json:"issuedAt"`
	// ExpiresAt is a Timestamp; nil means the token does not expire.
	ExpiresAt *string `json:"expiresAt"`
	// Repos are the repositories the token's permissions apply to
	// (owner/name). nil means GitHub did not tell; empty means none.
	Repos []string `json:"repos"`
}

// Timestamp writes a time the way the token records hold it: UTC with
// milliseconds, which is what the TypeScript version wrote.
func Timestamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// Expiry is when the token expires; false when it does not.
func (g GitHubState) Expiry() (time.Time, bool) {
	if g.ExpiresAt == nil {
		return time.Time{}, false
	}
	expiry, err := time.Parse(time.RFC3339Nano, *g.ExpiresAt)
	return expiry, err == nil
}

// DaysLeft is the number of whole days until the token expires, negative
// once it has; false when it does not expire.
func (g GitHubState) DaysLeft(now time.Time) (int, bool) {
	expiry, expires := g.Expiry()
	if !expires {
		return 0, false
	}
	return int(math.Floor(float64(expiry.Sub(now)) / float64(24*time.Hour))), true
}

// ExpiryWarnDays is how many days before the expiry a token counts as
// about to expire.
const ExpiryWarnDays = 7

// Urgency is "expired", "soon" (ExpiryWarnDays or less to go) or "".
func (g GitHubState) Urgency(now time.Time) string {
	left, expires := g.DaysLeft(now)
	switch {
	case !expires:
		return ""
	case left < 0:
		return "expired"
	case left <= ExpiryWarnDays:
		return "soon"
	}
	return ""
}

func (g GitHubState) validate() error {
	switch {
	case !ValidGitHubToken(g.Token):
		return errors.New(`"token" is not a fine-grained personal access token`)
	case !slices.Contains(GitHubPresets, g.Preset):
		return fmt.Errorf(`"preset" is %q; it is one of %s`, g.Preset, strings.Join(GitHubPresets, ", "))
	case g.Owner == "":
		return errors.New(`"owner" is empty`)
	}
	// Owner, name and repositories are shown on the terminal and written
	// into the environment as key=value lines.
	for _, text := range append([]string{g.Owner, g.TokenName, g.IssuedAt}, g.Repos...) {
		if controlChars.MatchString(text) {
			return errors.New("it contains control characters")
		}
	}
	if g.ExpiresAt != nil {
		if _, expires := g.Expiry(); !expires {
			return fmt.Errorf(`"expiresAt" is %q, which is not a time`, *g.ExpiresAt)
		}
	}
	return nil
}

// GitHubFile is the record of the environment's GitHub token.
func (s Store) GitHubFile(id string) string { return filepath.Join(s.EnvDir(id), "github.json") }

// LoadGitHub reads the environment's GitHub token. No file means no token
// (nil); a file that cannot be read is an error, never "no token": the
// token in it may still be valid at GitHub.
func (s Store) LoadGitHub(id string) (*GitHubState, error) {
	path := s.GitHubFile(id)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state GitHubState
	err = requireKeys(data, "token", "preset", "owner", "tokenName", "issuedAt", "expiresAt", "repos")
	if err == nil {
		err = decodeStrict(data, &state)
	}
	if err == nil {
		err = state.validate()
	}
	if err != nil {
		return nil, fmt.Errorf("%s is not a GitHub token record: %w\n(fix or delete the file and retry)", path, err)
	}
	return &state, nil
}

// SaveGitHub writes the environment's GitHub token.
func (s Store) SaveGitHub(id string, state GitHubState) error {
	if err := state.validate(); err != nil {
		return err
	}
	return writeSecretJSON(s.GitHubFile(id), state)
}

// RemoveGitHub deletes the record; none to delete is fine.
func (s Store) RemoveGitHub(id string) error {
	err := os.Remove(s.GitHubFile(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// githubClearPending marks an environment whose GitHub token was revoked
// while its volume could not be emptied. A revoked token left there would
// still be handed out as GH_TOKEN, and gh would not fall back to its own
// login; the next bh up empties the volume and removes the mark. (The name
// is the TypeScript version's, so a mark it left is honoured.)
func (s Store) githubClearPending(id string) string {
	return filepath.Join(s.EnvDir(id), "github.clear-pending")
}

// GitHubClearPending reports whether the mark is set.
func (s Store) GitHubClearPending(id string) bool {
	_, err := os.Stat(s.githubClearPending(id))
	return err == nil
}

// SetGitHubClearPending sets or removes the mark.
func (s Store) SetGitHubClearPending(id string, pending bool) error {
	path := s.githubClearPending(id)
	if pending {
		return os.WriteFile(path, nil, 0o600)
	}
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// requireKeys checks that the JSON object has every key. The decoder alone
// would take a missing "expiresAt" for "does not expire".
func requireKeys(data []byte, keys ...string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	for _, key := range keys {
		if _, present := object[key]; !present {
			return fmt.Errorf("%q is missing", key)
		}
	}
	return nil
}

// writeSecretJSON is writeJSON for a file that holds secrets: only the user
// can read it, from the first byte on.
func writeSecretJSON(path string, value any) error {
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	// The encoder would write <, > and & as \u00XX. The TypeScript version
	// does not, and the files are meant to be the same byte for byte.
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	temp := path + ".tmp"
	// The mode applies only when the file is created, so what an
	// interrupted run left behind goes first.
	if err := os.Remove(temp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = file.Write(data.Bytes())
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temp, path)
	}
	if err != nil {
		_ = os.Remove(temp)
	}
	return err
}
