package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Kinds of tokens other than GitHub's: a Hugging Face token, whose variable
// name is fixed, and one whose name the user picks.
const (
	TokenKindHF     = "hf"
	TokenKindCustom = "custom"
)

// TokenKinds are the kinds, in the order they are offered.
var TokenKinds = []string{TokenKindHF, TokenKindCustom}

// HFTokenName is the variable huggingface_hub reads.
const HFTokenName = "HF_TOKEN"

// TokenEntry is one token handed to an environment as an environment
// variable (tokens.json, mode 0600). The file is the TypeScript version's,
// field for field.
type TokenEntry struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Value   string `json:"value"`
	AddedAt string `json:"addedAt"`
}

type tokensFile struct {
	Tokens []TokenEntry `json:"tokens"`
}

// The rules for names and values come from how they reach the environment:
// one NAME=value line each, which bh-guest turns into export NAME='value'
// without escaping anything.
var (
	tokenNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	// Printable ASCII without the single quote.
	tokenValuePattern = regexp.MustCompile(`^[!-&(-~]+$`)
)

const (
	maxTokenName  = 40
	maxTokenValue = 4096
)

// reservedTokenNames cannot be set: names that break the start of a shell
// (zsh's special parameters — exporting a read-only one is an error, and
// assigning UID tries to change the user), names a login needs, and names
// bulkhead itself hands out. The rule lives on this machine only; the
// environment merely cannot inject them.
//
// Names that are just a way to shoot oneself in the foot (GIT_*,
// NODE_OPTIONS, *_PROXY) are not reserved: init.sh can set them as well.
var reservedTokenNames = []string{
	"GH_TOKEN", "GITHUB_TOKEN",
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

var reservedTokenPrefixes = []string{"LD_", "LC_", "DYLD_", "SSH_", "NIX_", "ZSH_", "ZLE_", "BULKHEAD_", "BH_"}

// CheckTokenName says what is wrong with a variable name for a token of
// the kind, in words that can be shown next to the input.
func CheckTokenName(kind, name string) error {
	switch kind {
	case TokenKindHF:
		if name != HFTokenName {
			return fmt.Errorf("Hugging Face tokens are always %s", HFTokenName)
		}
		return nil
	case TokenKindCustom:
	default:
		return fmt.Errorf("the kind of a token is one of %s (got %q)", strings.Join(TokenKinds, ", "), kind)
	}
	switch {
	case !tokenNamePattern.MatchString(name) || len(name) > maxTokenName:
		return fmt.Errorf("use A-Z, 0-9 and _ (starting with a letter, up to %d characters)", maxTokenName)
	case name == HFTokenName:
		return fmt.Errorf("%s is the Hugging Face token — add it as a Hugging Face token", HFTokenName)
	case name == "GH_TOKEN" || name == "GITHUB_TOKEN":
		return errors.New("the GitHub token has its own flow (bh github set)")
	case slices.Contains(reservedTokenNames, name) ||
		slices.ContainsFunc(reservedTokenPrefixes, func(prefix string) bool { return strings.HasPrefix(name, prefix) }):
		return fmt.Errorf("%s is reserved (the shell or bulkhead uses it)", name)
	}
	return nil
}

// CheckTokenValue says what is wrong with a token's value. The caller trims
// it first: a pasted value ends in a line break.
func CheckTokenValue(value string) error {
	switch {
	case value == "":
		return errors.New("the value is empty")
	case len(value) > maxTokenValue:
		return fmt.Errorf("the value is longer than %d characters", maxTokenValue)
	case !tokenValuePattern.MatchString(value):
		return errors.New("the value may use printable ASCII except ' (no spaces, quotes ' or control characters)")
	}
	return nil
}

// TokensFile is the record of the environment's tokens other than GitHub's.
func (s Store) TokensFile(id string) string { return filepath.Join(s.EnvDir(id), "tokens.json") }

// LoadTokens reads the environment's tokens. exists is false when there is
// no file: no token was ever set, and nothing was ever put in the volume.
// An existing file with no tokens is different — the volume is kept in line
// with it, which is how the last token leaves the environment.
func (s Store) LoadTokens(id string) (tokens []TokenEntry, exists bool, err error) {
	path := s.TokensFile(id)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var file tokensFile
	err = requireKeys(data, "tokens")
	if err == nil {
		err = decodeStrict(data, &file)
	}
	for _, entry := range file.Tokens {
		if err != nil {
			break
		}
		// Not the full rules for new tokens (the reserved names may have
		// grown since the entry was made) — only what keeps an entry from
		// becoming more than its own line in the environment.
		switch {
		case !slices.Contains(TokenKinds, entry.Kind):
			err = fmt.Errorf("%q is not a kind of token", entry.Kind)
		case !tokenNamePattern.MatchString(entry.Name):
			err = fmt.Errorf("%q is not a variable name", entry.Name)
		case !tokenValuePattern.MatchString(entry.Value):
			err = fmt.Errorf("the value of %s contains characters a token cannot have", entry.Name)
		case controlChars.MatchString(entry.AddedAt):
			err = fmt.Errorf("the entry %s contains control characters", entry.Name)
		}
	}
	if err != nil {
		return nil, false, fmt.Errorf("%s is not a token record: %w\n(fix or delete the file and retry)", path, err)
	}
	return file.Tokens, true, nil
}

// SaveTokens writes the environment's tokens as given. With none left the
// file stays, holding an empty list (see LoadTokens).
func (s Store) SaveTokens(id string, tokens []TokenEntry) error {
	return writeSecretJSON(s.TokensFile(id), tokensFile{Tokens: append([]TokenEntry{}, tokens...)})
}
