package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// LegacyDir resolves the configuration directory of the TypeScript version:
// BULKHEAD_CONFIG_DIR if set, else %APPDATA%\bulkhead on Windows and
// $XDG_CONFIG_HOME/bulkhead (default ~/.config/bulkhead) elsewhere.
func LegacyDir(goos string, getenv func(string) string) (string, error) {
	return dirNamed("bulkhead", "BULKHEAD_CONFIG_DIR", goos, getenv)
}

// LegacyStore is a configuration directory of the TypeScript version. bh
// reads it to migrate environments out of it, and changes exactly one thing
// in it: a migrated environment's directory moves from environments/ to
// environments.migrated/, where the TypeScript version no longer sees it.
// Nothing in it is ever deleted.
type LegacyStore struct{ Dir string }

func (l LegacyStore) environmentsDir() string { return filepath.Join(l.Dir, "environments") }
func (l LegacyStore) retiredDir() string      { return filepath.Join(l.Dir, "environments.migrated") }

// EnvDir is the directory of an environment the TypeScript version manages.
func (l LegacyStore) EnvDir(name string) string { return filepath.Join(l.environmentsDir(), name) }

// RetiredDir is where the directory of a migrated environment is kept.
func (l LegacyStore) RetiredDir(name string) string { return filepath.Join(l.retiredDir(), name) }

// MachineKey is the private half of the TypeScript version's machine key.
func (l LegacyStore) MachineKey() string { return filepath.Join(l.Dir, "id_ed25519") }

// Names lists the environments the TypeScript version still manages.
func (l LegacyStore) Names() ([]string, error) {
	entries, err := os.ReadDir(l.environmentsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(l.EnvDir(entry.Name()), "env.json")); err == nil {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	return names, nil
}

// Load reads one environment's settings. Its id is its name: the Docker
// volumes of the TypeScript version are named after the name, and keeping
// that as the id is what lets them be used as they are.
func (l LegacyStore) Load(name string) (Environment, error) {
	if !ValidID(name) {
		return Environment{}, fmt.Errorf("%q is not a valid environment name", name)
	}
	path := filepath.Join(l.EnvDir(name), "env.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Environment{}, fmt.Errorf("the TypeScript version has no environment named %q (looked in %s)", name, l.environmentsDir())
	}
	if err != nil {
		return Environment{}, err
	}
	env, err := ParseLegacyEnvironment(data, name)
	if err != nil {
		return Environment{}, fmt.Errorf("%s:\n%w", path, err)
	}
	return env, nil
}

// LegacyExtras is what an environment of the TypeScript version has besides
// its settings. Each of these is something bh must decide about before it
// takes the environment over.
type LegacyExtras struct {
	Override      bool // compose.override.yaml
	DevDockerfile bool // dev.Dockerfile
	GitHubToken   bool // github.json
	Tokens        bool // tokens.json
	InitScript    bool // a non-empty init.sh, shared by all environments
}

// Extras reports which extras the environment has.
func (l LegacyStore) Extras(name string) LegacyExtras {
	has := func(file string) bool {
		_, err := os.Stat(filepath.Join(l.EnvDir(name), file))
		return err == nil
	}
	init, _ := os.ReadFile(filepath.Join(l.Dir, "init.sh"))
	return LegacyExtras{
		Override:      has("compose.override.yaml"),
		DevDockerfile: has("dev.Dockerfile"),
		GitHubToken:   has("github.json"),
		Tokens:        has("tokens.json"),
		InitScript:    strings.TrimSpace(string(init)) != "",
	}
}

// LoadKeys reads the TypeScript version's key registry (the same format).
func (l LegacyStore) LoadKeys() ([]KeyEntry, error) {
	return loadKeys(filepath.Join(l.Dir, "keys.json"))
}

// Retire moves a migrated environment's directory out of the TypeScript
// version's sight. It is a rename: nothing is copied and nothing is deleted.
func (l LegacyStore) Retire(name string) error {
	return moveDir(l.EnvDir(name), l.RetiredDir(name))
}

// Restore undoes Retire.
func (l LegacyStore) Restore(name string) error {
	return moveDir(l.RetiredDir(name), l.EnvDir(name))
}

// Retired reports whether a migrated environment's directory is kept.
func (l LegacyStore) Retired(name string) bool {
	_, err := os.Stat(l.RetiredDir(name))
	return err == nil
}

// moveDir renames a directory and never replaces anything.
func moveDir(from, to string) error {
	if _, err := os.Lstat(to); err == nil {
		return fmt.Errorf("%s already exists; not moving %s over it", to, from)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.Rename(from, to)
}
