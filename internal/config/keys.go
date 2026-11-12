package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"

	"github.com/ys319/bulkhead/internal/sshkey"
)

// KeyEntry is a registered public key (keys.json). Environments refer to
// keys by fingerprint, so a reference also verifies the key it points at.
type KeyEntry struct {
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"publicKey"`
	Label       string `json:"label"` // display only
}

type keyRegistryFile struct {
	Keys []KeyEntry `json:"keys"`
}

// Keys are usually pasted from someone else; control characters in them
// would reach the terminal when they are listed.
var controlChars = regexp.MustCompile(`[\x00-\x1f\x7f-\x{9f}]`)

// LoadKeys reads the registry. No file means no keys; a file that cannot be
// read is an error for every command that needs it.
func (s Store) LoadKeys() ([]KeyEntry, error) { return loadKeys(s.KeyRegistry()) }

// AddKeys registers the keys that are not registered yet (by fingerprint)
// and leaves the existing entries as they are.
func (s Store) AddKeys(entries []KeyEntry) error {
	registry, err := s.LoadKeys()
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, entry := range registry {
		known[entry.Fingerprint] = true
	}
	added := false
	for _, entry := range entries {
		if !known[entry.Fingerprint] {
			registry, known[entry.Fingerprint], added = append(registry, entry), true, true
		}
	}
	if !added {
		return nil
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	return writeJSON(s.KeyRegistry(), keyRegistryFile{Keys: registry})
}

func loadKeys(path string) ([]KeyEntry, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var registry keyRegistryFile
	if err := decodeStrict(data, &registry); err != nil {
		return nil, fmt.Errorf("%s: %w\n(fix the file and retry)", path, err)
	}
	for _, entry := range registry.Keys {
		switch {
		case !fingerprintPattern.MatchString(entry.Fingerprint):
			return nil, fmt.Errorf("%s: %q is not a key fingerprint\n(fix the file and retry)", path, entry.Fingerprint)
		case controlChars.MatchString(entry.PublicKey), controlChars.MatchString(entry.Label):
			return nil, fmt.Errorf("%s: the entry %s contains control characters\n(fix the file and retry)", path, entry.Fingerprint)
		}
	}
	return registry.Keys, nil
}

// ResolveKeys turns an environment's key references into public key lines.
// A reference that is not registered, or an entry whose key does not match
// its fingerprint, is an error: dropping a key silently would only show up
// as "I cannot log in any more".
func ResolveKeys(registry []KeyEntry, env Environment) ([]string, error) {
	byFingerprint := map[string]KeyEntry{}
	for _, entry := range registry {
		byFingerprint[entry.Fingerprint] = entry
	}
	keys := make([]string, 0, len(env.AuthorizedKeys))
	for _, ref := range env.AuthorizedKeys {
		entry, found := byFingerprint[ref]
		if !found {
			return nil, fmt.Errorf("environment %q refers to key %s, which is not registered (keys.json)", env.Name, ref)
		}
		actual, err := sshkey.Fingerprint(entry.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("registered key %s: %w", ref, err)
		}
		if actual != ref {
			return nil, fmt.Errorf("registered key %s does not match its public key (which is %s) — keys.json was edited or damaged", ref, actual)
		}
		keys = append(keys, entry.PublicKey)
	}
	return keys, nil
}
