package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
)

// GitHubPresets are the permission sets a GitHub token can be issued with,
// by name, each including the one before. There is no default among them:
// the choice is made every time.
//
//	read        clone and fetch private repositories
//	push        also push, and read the results of CI
//	contribute  also open, update and review pull requests
var GitHubPresets = []string{"read", "push", "contribute"}

// A fine-grained personal access token. bh-guest accepts the same
// characters and nothing else.
var githubTokenPattern = regexp.MustCompile(`^github_pat_[A-Za-z0-9_]+$`)

// ValidGitHubToken reports whether text has the form of a fine-grained
// personal access token. Whether GitHub accepts it is another question.
func ValidGitHubToken(text string) bool { return githubTokenPattern.MatchString(text) }

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

// writeSecretJSON is writeJSON for a file about credentials: only the user
// can read it, from the first byte on.
func writeSecretJSON(path string, value any) error {
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
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
