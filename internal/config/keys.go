package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"

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

// SaveKeys writes the registry as given.
func (s Store) SaveKeys(registry []KeyEntry) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	return writeJSON(s.KeyRegistry(), keyRegistryFile{Keys: append([]KeyEntry{}, registry...)})
}

var (
	keyTypePattern = regexp.MustCompile(`^(ssh|ecdsa|sk)-[a-z0-9@.-]+$`)
	keyBodyPattern = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)
	// What a second key pasted into the comment looks like.
	keyInComment = regexp.MustCompile(`(ssh|ecdsa|sk)-[a-z0-9@.-]+\s+[A-Za-z0-9+/]{16}`)
	// Line breaks and tabs are folded away; every other control character
	// is refused (an escape sequence in a comment would reach the terminal).
	keyLineControlChars = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f-\x{9f}]`)
)

// ParsePublicKey turns pasted or piped input into one authorized_keys line.
// It is not a cryptographic check (the fingerprint is): the point is to
// catch the wrong file, and above all a private key.
func ParsePublicKey(input string) (string, error) {
	if strings.Contains(input, "PRIVATE KEY") {
		return "", errors.New("this looks like a PRIVATE key — never share it; pass the .pub file instead")
	}
	if keyLineControlChars.MatchString(input) {
		return "", errors.New("the public key contains control characters (for example terminal escape sequences) — pass the .pub file as it is")
	}
	var lines []string
	for _, line := range strings.Split(input, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		return "", fmt.Errorf("expected exactly one public key line, got %d", len(lines))
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 2 || !keyTypePattern.MatchString(fields[0]) {
		return "", fmt.Errorf("not an SSH public key: %q (expected something like \"ssh-ed25519 AAAA... comment\")", lines[0])
	}
	if !keyBodyPattern.MatchString(fields[1]) {
		return "", fmt.Errorf("the key body is not base64: %q", fields[1])
	}
	if keyInComment.MatchString(strings.Join(fields[2:], " ")) {
		return "", errors.New("the comment contains what looks like another public key — paste exactly one key")
	}
	return strings.Join(fields, " "), nil
}

// NormalizeLabel checks a label for a registered key.
func NormalizeLabel(label string) (string, error) {
	label = strings.TrimSpace(label)
	switch {
	case controlChars.MatchString(label):
		return "", errors.New("key labels cannot contain control characters")
	case len([]rune(label)) > 64:
		return "", errors.New("key labels are limited to 64 characters")
	}
	return label, nil
}

// DisplayName is the label, or the tail of the key when it has none.
func (k KeyEntry) DisplayName() string {
	if k.Label != "" {
		return k.Label
	}
	fields := strings.Fields(k.PublicKey)
	if len(fields) < 2 {
		return "..."
	}
	body := fields[1]
	if len(body) > 8 {
		body = body[len(body)-8:]
	}
	return "..." + body
}

// NewKeyEntry makes the registry entry for a public key line. Without a
// label the key's comment is used.
func NewKeyEntry(publicKey, label string, labelGiven bool) (KeyEntry, error) {
	fingerprint, err := sshkey.Fingerprint(publicKey)
	if err != nil {
		return KeyEntry{}, err
	}
	if !labelGiven {
		if fields := strings.Fields(publicKey); len(fields) > 2 {
			label = strings.Join(fields[2:], " ")
		}
	}
	label, err = NormalizeLabel(label)
	if err != nil {
		return KeyEntry{}, err
	}
	return KeyEntry{Fingerprint: fingerprint, PublicKey: publicKey, Label: label}, nil
}

const fingerprintPrefix = "SHA256:"

// matchesKey: a reference is a label, or a fingerprint or the start of one
// (with or without "SHA256:").
func matchesKey(fingerprint, label, ref string) bool {
	prefix := strings.TrimPrefix(ref, fingerprintPrefix)
	return (label != "" && label == ref) ||
		(prefix != "" && strings.HasPrefix(strings.TrimPrefix(fingerprint, fingerprintPrefix), prefix))
}

// FindKey resolves a reference to exactly one registered key.
func FindKey(registry []KeyEntry, ref string) (KeyEntry, error) {
	if len(registry) == 0 {
		return KeyEntry{}, errors.New("no keys are registered — register one with bh key add")
	}
	var hits []KeyEntry
	for _, entry := range registry {
		if matchesKey(entry.Fingerprint, entry.Label, ref) {
			hits = append(hits, entry)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return KeyEntry{}, fmt.Errorf("no registered key matches %q (see bh key list)", ref)
	}
	lines := []string{fmt.Sprintf("%q is ambiguous — it matches:", ref)}
	for _, hit := range hits {
		lines = append(lines, "  "+hit.Fingerprint+"  "+hit.DisplayName())
	}
	return KeyEntry{}, errors.New(strings.Join(lines, "\n"))
}

// FindAttached resolves a reference among the keys attached to env, which
// may include fingerprints that are no longer registered.
func FindAttached(registry []KeyEntry, env Environment, ref string) (string, error) {
	labels := map[string]string{}
	for _, entry := range registry {
		labels[entry.Fingerprint] = entry.Label
	}
	var hits []string
	for _, fingerprint := range env.AuthorizedKeys {
		if matchesKey(fingerprint, labels[fingerprint], ref) && !slices.Contains(hits, fingerprint) {
			hits = append(hits, fingerprint)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return "", fmt.Errorf("no key attached to %q matches %q (see bh key list %s)", env.Name, ref, env.Name)
	}
	return "", fmt.Errorf("%q is ambiguous on %q: %s", ref, env.Name, strings.Join(hits, ", "))
}

// UsersOf are the names of the local environments that let the key in.
func (c *Config) UsersOf(fingerprint string) []string {
	var names []string
	for _, env := range c.Envs {
		if env.Local() && slices.Contains(env.AuthorizedKeys, fingerprint) {
			names = append(names, env.Name)
		}
	}
	return names
}
