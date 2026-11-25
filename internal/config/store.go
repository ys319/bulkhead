// Package config reads and writes the configuration directory: which
// environments exist and what their settings are.
//
//	<dir>/bulkhead.json                 marks the directory and holds its id
//	<dir>/settings.json                 what holds for all environments (once set)
//	<dir>/run/<id>/forward-<name>.lock  held by the bh that has those forwards open
//	<dir>/environments/<id>/env.json    one environment's settings
//	<dir>/id_ed25519[.pub]              the machine's ssh key
//	<dir>/keys.json                     registered public keys
package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// FormatVersion is the version this build writes into bulkhead.json and
// env.json and the only one it reads.
const FormatVersion = 1

// Dir resolves the configuration directory: BH_CONFIG_DIR if set, else
// %APPDATA%\bh on Windows and $XDG_CONFIG_HOME/bh (default ~/.config/bh)
// elsewhere.
//
// It is deliberately not the directory of the TypeScript version (named
// "bulkhead", overridden by BULKHEAD_CONFIG_DIR — see LegacyDir): the two
// versions never read each other's files, and an environment moves from one
// to the other only through `bh migrate`.
func Dir(goos string, getenv func(string) string) (string, error) {
	return dirNamed("bh", "BH_CONFIG_DIR", goos, getenv)
}

func dirNamed(name, override, goos string, getenv func(string) string) (string, error) {
	if explicit := getenv(override); explicit != "" {
		return explicit, nil
	}
	if goos == "windows" {
		appData := getenv("APPDATA")
		if appData == "" {
			return "", errors.New("APPDATA is not set")
		}
		return filepath.Join(appData, name), nil
	}
	if xdg := getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, name), nil
	}
	home := getenv("HOME")
	if home == "" {
		return "", errors.New("HOME is not set")
	}
	return filepath.Join(home, ".config", name), nil
}

// Store is a configuration directory.
type Store struct{ Dir string }

func (s Store) rootFile() string        { return filepath.Join(s.Dir, "bulkhead.json") }
func (s Store) environmentsDir() string { return filepath.Join(s.Dir, "environments") }

// EnvDir is the directory of one environment. Its name is the environment's
// id, so it never moves when the environment is renamed and stays findable
// when env.json is unreadable.
func (s Store) EnvDir(id string) string { return filepath.Join(s.environmentsDir(), id) }

// EnvFile is the settings file of one environment.
func (s Store) EnvFile(id string) string { return filepath.Join(s.EnvDir(id), "env.json") }

// DevDockerfile is the optional per-environment extension of the dev image.
func (s Store) DevDockerfile(id string) string { return filepath.Join(s.EnvDir(id), "dev.Dockerfile") }

// SnapshotsDir holds the snapshots of one environment. It is not inside
// the environment's own directory: that one is the build context of its
// dev.Dockerfile, and it goes when the environment is destroyed — the
// snapshots stay, as the way back from a destroy.
func (s Store) SnapshotsDir(id string) string { return filepath.Join(s.Dir, "snapshots", id) }

// SnapshotsRoot holds the snapshot directories of all environments.
func (s Store) SnapshotsRoot() string { return filepath.Join(s.Dir, "snapshots") }

// ForwardLock is the file whose lock says that a bh has the environment's
// saved forwards of that name open. It is not in the environment's own
// directory, which is the build context of its dev.Dockerfile.
func (s Store) ForwardLock(id, name string) string {
	return filepath.Join(s.Dir, "run", id, "forward-"+name+".lock")
}

// MachineKey is the private half of the machine's ssh key.
func (s Store) MachineKey() string { return filepath.Join(s.Dir, "id_ed25519") }

// KeyRegistry is the file of registered public keys.
func (s Store) KeyRegistry() string { return filepath.Join(s.Dir, "keys.json") }

// InitScript is the init script shared by all environments: it runs once in
// a fresh home volume. The file is the user's to edit.
func (s Store) InitScript() string { return filepath.Join(s.Dir, "init.sh") }

// ErrLegacy means the directory holds environments but no bulkhead.json: it
// belongs to the TypeScript version and has not been migrated.
var ErrLegacy = errors.New("this configuration directory belongs to the TypeScript version of bulkhead")

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidID reports whether id can identify an environment (and with it name
// Docker resources and a directory).
func ValidID(id string) bool { return idPattern.MatchString(id) && !reservedDeviceName.MatchString(id) }

// NewID returns a fresh random id: 12 hex digits.
func NewID() (string, error) {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

type rootFile struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	// SecretStore is where this machine keeps the values of tokens and
	// secrets ("keychain" or "file"); absent until the first one is set.
	SecretStore string `json:"secretStore,omitempty"`
}

// Owner is the id of this directory, as put on the Docker resources it
// creates. It is "" when the directory has not been initialized (and so
// owns nothing).
func (s Store) Owner() (string, error) {
	data, err := os.ReadFile(s.rootFile())
	if errors.Is(err, fs.ErrNotExist) {
		if s.hasLegacyEnvironments() {
			return "", s.legacyError()
		}
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var root rootFile
	if err := decodeStrict(data, &root); err != nil {
		return "", fmt.Errorf("%s: %w", s.rootFile(), err)
	}
	if root.Version != FormatVersion {
		return "", fmt.Errorf("%s has version %d; this bh reads version %d", s.rootFile(), root.Version, FormatVersion)
	}
	if !ValidID(root.ID) {
		return "", fmt.Errorf("%s: %q is not a valid id", s.rootFile(), root.ID)
	}
	return root.ID, nil
}

// readRoot reads bulkhead.json; a directory that is not initialized has an
// empty one.
func (s Store) readRoot() (rootFile, error) {
	var root rootFile
	data, err := os.ReadFile(s.rootFile())
	if errors.Is(err, fs.ErrNotExist) {
		return root, nil
	}
	if err != nil {
		return root, err
	}
	if err := decodeStrict(data, &root); err != nil {
		return root, fmt.Errorf("%s: %w", s.rootFile(), err)
	}
	return root, nil
}

// SecretStore is where this machine keeps the values of tokens and
// secrets: "keychain", "file", or "" while that is not decided.
func (s Store) SecretStore() (string, error) {
	root, err := s.readRoot()
	return root.SecretStore, err
}

// SetSecretStore records the decision. The directory is initialized first
// if need be.
func (s Store) SetSecretStore(kind string) error {
	if kind != "keychain" && kind != "file" {
		return fmt.Errorf("%q is not a place for secrets (keychain or file)", kind)
	}
	if _, err := s.Init(); err != nil {
		return err
	}
	root, err := s.readRoot()
	if err != nil {
		return err
	}
	root.SecretStore = kind
	return writeJSON(s.rootFile(), root)
}

// Init makes sure the directory is initialized and returns its owner id.
func (s Store) Init() (string, error) {
	owner, err := s.Owner()
	if err != nil || owner != "" {
		return owner, err
	}
	owner, err = NewID()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return "", err
	}
	if err := writeJSON(s.rootFile(), rootFile{Version: FormatVersion, ID: owner}); err != nil {
		return "", err
	}
	return owner, nil
}

func (s Store) legacyError() error {
	return fmt.Errorf("%w: %s\n(bh keeps its own directory; move environments over one at a time with bh migrate)", ErrLegacy, s.Dir)
}

func (s Store) hasLegacyEnvironments() bool {
	entries, err := os.ReadDir(s.environmentsDir())
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(s.EnvFile(entry.Name())); err == nil {
			return true
		}
	}
	return false
}

// SaveEnvironment writes the environment's settings, creating its directory.
func (s Store) SaveEnvironment(env Environment) error {
	if !ValidID(env.ID) {
		return fmt.Errorf("%q is not a valid environment id", env.ID)
	}
	if err := env.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(s.EnvDir(env.ID), 0o755); err != nil {
		return err
	}
	return writeJSON(s.EnvFile(env.ID), env.file())
}

// RemoveEnvironment deletes the environment's directory and all in it.
func (s Store) RemoveEnvironment(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("%q is not a valid environment id", id)
	}
	// The locks of its forwards: files that mean nothing without it.
	if err := os.RemoveAll(filepath.Join(s.Dir, "run", id)); err != nil {
		return err
	}
	return os.RemoveAll(s.EnvDir(id))
}

// writeJSON replaces the file in one step (temp file + rename), so a crash
// never leaves half a file behind.
func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

// decodeStrict decodes one JSON value and refuses unknown fields and
// anything after the value: a typo in a hand-edited file must not be
// silently ignored.
func decodeStrict(data []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("unexpected content after the JSON value")
	}
	return nil
}
