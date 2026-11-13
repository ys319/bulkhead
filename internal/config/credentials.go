package config

import (
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
	"unicode/utf8"
)

// What an environment is handed comes by three routes with three meanings
// (spec draft §7):
//
//	var     an environment variable that is no secret (env.json)
//	secret  a secret value the user has (secrets.json)
//	token   a credential a service issued for this environment, which bh
//	        knows how to issue, check, renew and revoke (tokens.json)
//
// The files hold everything but the values: those are in the vault
// (internal/vault), under the keys TokenKey and SecretKey give.

// Services bh can get a token from.
const (
	ServiceGitHub      = "github"
	ServiceHuggingFace = "huggingface"
)

// TokenKey and SecretKey are the vault's keys for the values.
func TokenKey(service string) string { return "token/" + service }
func SecretKey(name string) string   { return "secret/" + name }

// Token is a credential a service issued for the environment, without its
// value.
type Token struct {
	Service string `json:"service"`
	// Preset is what bh asked the service's page for; empty for a service
	// that has no presets.
	Preset string `json:"preset,omitempty"`
	// Access is what the service says the token may do, where it says so
	// (Hugging Face: read, write or fineGrained).
	Access string `json:"access,omitempty"`
	// Owner is the account the token belongs to.
	Owner string `json:"owner,omitempty"`
	// TokenName is the name the token has on the service's page: what to
	// look for there.
	TokenName string `json:"tokenName,omitempty"`
	IssuedAt  string `json:"issuedAt"`
	// ExpiresAt is a Timestamp; nil means the token does not expire.
	ExpiresAt *string `json:"expiresAt"`
	// Repos are the repositories the token reaches (owner/name). nil means
	// the service did not tell; empty means none.
	Repos []string `json:"repos"`
}

// Timestamp writes a time the way the records hold it: UTC with
// milliseconds, which is what the TypeScript version wrote.
func Timestamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// Expiry is when the token expires; false when it does not.
func (t Token) Expiry() (time.Time, bool) {
	if t.ExpiresAt == nil {
		return time.Time{}, false
	}
	expiry, err := time.Parse(time.RFC3339Nano, *t.ExpiresAt)
	return expiry, err == nil
}

// DaysLeft is the number of whole days until the token expires, negative
// once it has; false when it does not expire.
func (t Token) DaysLeft(now time.Time) (int, bool) {
	expiry, expires := t.Expiry()
	if !expires {
		return 0, false
	}
	return int(math.Floor(float64(expiry.Sub(now)) / float64(24*time.Hour))), true
}

// ExpiryWarnDays is how many days before the expiry a token counts as
// about to expire.
const ExpiryWarnDays = 7

// Urgency is "expired", "soon" (ExpiryWarnDays or less to go) or "".
func (t Token) Urgency(now time.Time) string {
	left, expires := t.DaysLeft(now)
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

var serviceName = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}$`)

func (t Token) validate() error {
	if !serviceName.MatchString(t.Service) {
		return fmt.Errorf("%q is not the name of a service", t.Service)
	}
	// All of it is shown on the terminal, and some is written into the
	// environment as key=value lines.
	for _, text := range append([]string{t.Preset, t.Access, t.Owner, t.TokenName, t.IssuedAt}, t.Repos...) {
		if controlChars.MatchString(text) {
			return fmt.Errorf("the %s token's record contains control characters", t.Service)
		}
	}
	if t.ExpiresAt != nil {
		if _, expires := t.Expiry(); !expires {
			return fmt.Errorf(`the %s token's "expiresAt" is not a time`, t.Service)
		}
	}
	return nil
}

// Secret is a secret value the environment gets as an environment
// variable, without the value.
type Secret struct {
	Name    string `json:"name"`
	AddedAt string `json:"addedAt"`
}

// The rules for the names and values of variables and secrets come from
// how they reach the environment: one NAME=value line each, which bh-guest
// turns into an export with the value quoted.
var (
	valueName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

const (
	maxValueName = 64
	maxValue     = 16384
)

// reservedNames cannot be set: names that break the start of a shell
// (zsh's special parameters — exporting a read-only one is an error, and
// assigning UID tries to change the user), names a login needs, and names
// that are a token's. The rule lives on this machine only; the environment
// merely cannot inject them.
//
// Names that are just a way to shoot oneself in the foot (GIT_*,
// NODE_OPTIONS, *_PROXY) are not reserved: init.sh can set them as well.
var reservedNames = []string{
	// Login and the basics of the environment.
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "LANG", "TMPDIR", "ENV", "BASH_ENV", "EDITOR", "VISUAL",
	// zsh's special parameters (zshparam: set by the shell / used by the shell).
	"ARGC", "ARGV0", "CDPATH", "COLUMNS", "CPUTYPE", "EGID", "ERRNO", "EUID", "FCEDIT", "FIGNORE", "FPATH",
	"GID", "HISTCHARS", "HISTCMD", "HISTFILE", "HISTSIZE", "HOST", "IFS", "KEYBOARD_HACK", "KEYTIMEOUT",
	"LINENO", "LINES", "LISTMAX", "LOGCHECK", "MACHTYPE", "MAIL", "MAILCHECK", "MAILPATH", "MANPATH",
	"MODULE_PATH", "NULLCMD", "OLDPWD", "OPTARG", "OPTIND", "OSTYPE", "POSTEDIT", "PPID", "PROMPT", "PROMPT2",
	"PROMPT3", "PROMPT4", "PS1", "PS2", "PS3", "PS4", "PSVAR", "PWD", "RANDOM", "READNULLCMD", "REPLY",
	"RPROMPT", "RPROMPT2", "RPS1", "RPS2", "SAVEHIST", "SECONDS", "SHLVL", "SIGNAL", "SPROMPT", "STTY",
	"TERMINFO", "TERMINFO_DIRS", "TIMEFMT", "TMOUT", "TMPPREFIX", "TRY_BLOCK_ERROR", "TRY_BLOCK_INTERRUPT",
	"TTY", "TTYIDLE", "UID", "USERNAME", "VENDOR", "WATCH", "WATCHFMT", "WORDCHARS", "ZBEEP", "ZDOTDIR",
}

var reservedPrefixes = []string{"LD_", "LC_", "DYLD_", "SSH_", "NIX_", "ZSH_", "ZLE_", "BULKHEAD_", "BH_"}

// TokenVariables are the names tokens come under in the environment, and
// the service each belongs to. A variable or a secret cannot take them: a
// token goes the token's route (bh token set).
var TokenVariables = map[string]string{
	"GH_TOKEN":     ServiceGitHub,
	"GITHUB_TOKEN": ServiceGitHub,
	"HF_TOKEN":     ServiceHuggingFace,
}

// CheckName says what is wrong with the name of a variable or a secret, in
// words that can be shown next to the input.
func CheckName(name string) error {
	switch {
	case !valueName.MatchString(name) || len(name) > maxValueName:
		return fmt.Errorf("use letters, digits and _ (not starting with a digit, up to %d characters)", maxValueName)
	case TokenVariables[name] != "":
		return fmt.Errorf("%s is the %s token's — that is set with bh token set", name, TokenVariables[name])
	case slices.Contains(reservedNames, name) ||
		slices.ContainsFunc(reservedPrefixes, func(prefix string) bool { return strings.HasPrefix(name, prefix) }):
		return fmt.Errorf("%s is reserved (the shell or bulkhead uses it)", name)
	}
	return nil
}

// CheckValue says what is wrong with the value of a variable or a secret:
// it is one line of text. The caller trims a pasted secret first (it ends
// in a line break).
func CheckValue(value string) error {
	switch {
	case value == "":
		return errors.New("the value is empty")
	case len(value) > maxValue:
		return fmt.Errorf("the value is longer than %d bytes", maxValue)
	case !utf8.ValidString(value) || controlChars.MatchString(value):
		return errors.New("the value is one line of text (no line breaks, tabs or other control characters)")
	}
	return nil
}

// recordVersion is the version of tokens.json and secrets.json. Earlier
// builds and the TypeScript version wrote a tokens.json without one.
const recordVersion = 1

type tokensFile struct {
	Version int     `json:"version"`
	Tokens  []Token `json:"tokens"`
}

type secretsFile struct {
	Version int      `json:"version"`
	Secrets []Secret `json:"secrets"`
}

// TokensFile, SecretsFile and ValuesFile are the environment's records of
// its tokens and secrets, and the file their values are in when this
// machine keeps them in files.
func (s Store) TokensFile(id string) string  { return filepath.Join(s.EnvDir(id), "tokens.json") }
func (s Store) SecretsFile(id string) string { return filepath.Join(s.EnvDir(id), "secrets.json") }
func (s Store) ValuesFile(id string) string  { return filepath.Join(s.EnvDir(id), "values.json") }

// readRecord reads a record file into file. It is false when there is no
// file. A file that cannot be read is an error, never "nothing there": the
// credentials it names may still be valid at their services.
func readRecord(path string, file any) (exists bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := requireKeys(data, "version"); err != nil {
		return true, fmt.Errorf("%s: %w", path, err)
	}
	if err := decodeStrict(data, file); err != nil {
		return true, fmt.Errorf("%s: %w\n(fix or delete the file and retry)", path, err)
	}
	return true, nil
}

// LoadTokens reads the environment's tokens; none without a file.
func (s Store) LoadTokens(id string) ([]Token, error) {
	var file tokensFile
	path := s.TokensFile(id)
	if exists, err := readRecord(path, &file); err != nil || !exists {
		return nil, err
	}
	if file.Version != recordVersion {
		return nil, fmt.Errorf("%s has version %d; this bh reads version %d", path, file.Version, recordVersion)
	}
	seen := map[string]bool{}
	for _, token := range file.Tokens {
		if err := token.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w\n(fix or delete the file and retry)", path, err)
		}
		if seen[token.Service] {
			return nil, fmt.Errorf("%s names the %s token twice", path, token.Service)
		}
		seen[token.Service] = true
	}
	return file.Tokens, nil
}

// SaveTokens writes the environment's tokens; with none, the file goes.
func (s Store) SaveTokens(id string, tokens []Token) error {
	if len(tokens) == 0 {
		return removeIfThere(s.TokensFile(id))
	}
	for _, token := range tokens {
		if err := token.validate(); err != nil {
			return err
		}
	}
	return writeSecretJSON(s.TokensFile(id), tokensFile{Version: recordVersion, Tokens: tokens})
}

// LoadSecrets reads the names of the environment's secrets.
func (s Store) LoadSecrets(id string) ([]Secret, error) {
	var file secretsFile
	path := s.SecretsFile(id)
	if exists, err := readRecord(path, &file); err != nil || !exists {
		return nil, err
	}
	if file.Version != recordVersion {
		return nil, fmt.Errorf("%s has version %d; this bh reads version %d", path, file.Version, recordVersion)
	}
	for _, secret := range file.Secrets {
		// Not the full rules for new names (the reserved ones may have grown
		// since) — only what keeps an entry from becoming more than its own
		// line in the environment.
		if !valueName.MatchString(secret.Name) || controlChars.MatchString(secret.AddedAt) {
			return nil, fmt.Errorf("%s: %q is not the name of a secret\n(fix or delete the file and retry)", path, secret.Name)
		}
	}
	return file.Secrets, nil
}

// SaveSecrets writes the names of the environment's secrets; with none,
// the file goes.
func (s Store) SaveSecrets(id string, secrets []Secret) error {
	if len(secrets) == 0 {
		return removeIfThere(s.SecretsFile(id))
	}
	return writeSecretJSON(s.SecretsFile(id), secretsFile{Version: recordVersion, Secrets: secrets})
}

func removeIfThere(path string) error {
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Marks that a volume still holds what the records no longer have: a
// token or the last variable was taken away while the volume could not be
// written. What is left there would still be handed out; the next bh up
// brings the volume in line and removes the mark. (github.clear-pending is
// the TypeScript version's name, so a mark it left is honoured.)
const (
	PendingGitHub = "github.clear-pending"
	PendingEnv    = "env.clear-pending"
)

// Pending reports whether the mark is set.
func (s Store) Pending(id, mark string) bool {
	_, err := os.Stat(filepath.Join(s.EnvDir(id), mark))
	return err == nil
}

// SetPending sets or removes the mark.
func (s Store) SetPending(id, mark string, pending bool) error {
	path := filepath.Join(s.EnvDir(id), mark)
	if pending {
		return os.WriteFile(path, nil, 0o600)
	}
	return removeIfThere(path)
}

// Records of earlier builds and of the TypeScript version: github.json
// (the GitHub token with its value) and a tokens.json without a version
// (Hugging Face tokens and variables the user named, with their values).

type legacyGitHub struct {
	Token     string   `json:"token"`
	Preset    string   `json:"preset"`
	Owner     string   `json:"owner"`
	TokenName string   `json:"tokenName"`
	IssuedAt  string   `json:"issuedAt"`
	ExpiresAt *string  `json:"expiresAt"`
	Repos     []string `json:"repos"`
}

type legacyTokens struct {
	Tokens []struct {
		Kind    string `json:"kind"`
		Name    string `json:"name"`
		Value   string `json:"value"`
		AddedAt string `json:"addedAt"`
	} `json:"tokens"`
}

// legacyPresets maps the preset names of earlier builds to today's.
var legacyPresets = map[string]string{"read": "read", "push": "push", "push+pr": "contribute"}

func (s Store) legacyGitHubFile(id string) string { return filepath.Join(s.EnvDir(id), "github.json") }

// hasLegacyTokens: tokens.json is there and has no version.
func (s Store) hasLegacyTokens(id string) (bool, error) {
	data, err := os.ReadFile(s.TokensFile(id))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return false, fmt.Errorf("%s: %w", s.TokensFile(id), err)
	}
	_, versioned := object["version"]
	return !versioned, nil
}

// HasLegacyCredentials reports whether the environment's directory holds
// records in the earlier form, which ConvertLegacyCredentials takes over.
func (s Store) HasLegacyCredentials(id string) (bool, error) {
	if _, err := os.Stat(s.legacyGitHubFile(id)); err == nil {
		return true, nil
	}
	return s.hasLegacyTokens(id)
}

// ConvertLegacyCredentials turns the records of earlier builds into
// today's: the values go to put (the vault), the rest into tokens.json and
// secrets.json, and github.json goes. It can be run again after a failure:
// until github.json and the unversioned tokens.json are gone, it starts
// over from them.
func (s Store) ConvertLegacyCredentials(id string, put func(key, value string) error) error {
	var tokens []Token
	var secrets []Secret
	legacy, err := s.hasLegacyTokens(id)
	if err != nil {
		return err
	}
	if !legacy {
		// A run that stopped after tokens.json was rewritten: go on from it.
		if tokens, err = s.LoadTokens(id); err != nil {
			return err
		}
		if secrets, err = s.LoadSecrets(id); err != nil {
			return err
		}
	}
	replace := func(token Token) {
		tokens = slices.DeleteFunc(tokens, func(t Token) bool { return t.Service == token.Service })
		tokens = append(tokens, token)
	}

	githubPath := s.legacyGitHubFile(id)
	if data, err := os.ReadFile(githubPath); err == nil {
		var old legacyGitHub
		if err := requireKeys(data, "token", "preset", "owner", "tokenName", "issuedAt", "expiresAt", "repos"); err != nil {
			return fmt.Errorf("%s is not a GitHub token record: %w\n(fix or delete the file and retry)", githubPath, err)
		}
		if err := decodeStrict(data, &old); err != nil {
			return fmt.Errorf("%s is not a GitHub token record: %w\n(fix or delete the file and retry)", githubPath, err)
		}
		preset, known := legacyPresets[old.Preset]
		if !known || !ValidGitHubToken(old.Token) || old.Owner == "" {
			return fmt.Errorf("%s is not a GitHub token record\n(fix or delete the file and retry)", githubPath)
		}
		token := Token{
			Service: ServiceGitHub, Preset: preset, Owner: old.Owner, TokenName: old.TokenName,
			IssuedAt: old.IssuedAt, ExpiresAt: old.ExpiresAt, Repos: old.Repos,
		}
		if err := token.validate(); err != nil {
			return fmt.Errorf("%s: %w", githubPath, err)
		}
		if err := put(TokenKey(ServiceGitHub), old.Token); err != nil {
			return err
		}
		replace(token)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	if legacy {
		data, err := os.ReadFile(s.TokensFile(id))
		if err != nil {
			return err
		}
		var old legacyTokens
		if err := decodeStrict(data, &old); err != nil {
			return fmt.Errorf("%s is not a token record: %w\n(fix or delete the file and retry)", s.TokensFile(id), err)
		}
		for _, entry := range old.Tokens {
			if controlChars.MatchString(entry.AddedAt) || CheckValue(entry.Value) != nil {
				return fmt.Errorf("%s: the entry %q cannot be taken over\n(fix or delete the file and retry)", s.TokensFile(id), entry.Name)
			}
			switch entry.Kind {
			case "hf":
				if err := put(TokenKey(ServiceHuggingFace), entry.Value); err != nil {
					return err
				}
				replace(Token{Service: ServiceHuggingFace, IssuedAt: entry.AddedAt})
			case "custom":
				if !valueName.MatchString(entry.Name) {
					return fmt.Errorf("%s: %q is not a variable name\n(fix or delete the file and retry)", s.TokensFile(id), entry.Name)
				}
				if err := put(SecretKey(entry.Name), entry.Value); err != nil {
					return err
				}
				secrets = slices.DeleteFunc(secrets, func(s Secret) bool { return s.Name == entry.Name })
				secrets = append(secrets, Secret{Name: entry.Name, AddedAt: entry.AddedAt})
			default:
				return fmt.Errorf("%s: %q is not a kind of token\n(fix or delete the file and retry)", s.TokensFile(id), entry.Kind)
			}
		}
	}

	// The values are in the vault. Now the records: secrets first, then
	// tokens.json — rewriting it is what ends its being a legacy file — and
	// github.json last.
	if err := s.SaveSecrets(id, secrets); err != nil {
		return err
	}
	if len(tokens) == 0 {
		if err := removeIfThere(s.TokensFile(id)); err != nil {
			return err
		}
	} else if err := s.SaveTokens(id, tokens); err != nil {
		return err
	}
	return removeIfThere(githubPath)
}
