// Package vault keeps the values of tokens and secrets on this machine:
// in the operating system's credential store where there is one, in a file
// only the user can read where there is not. What a value is the value of
// — names, services, expiry — is not here but in the environment's
// records (internal/config), so that listing and checking never touch the
// store.
//
// The credential store keeps values out of files: out of backups, synced
// directories and the output of grep. It is not a wall against a program
// running as the same user — on macOS such a program can ask the keychain
// the same way bh does.
package vault

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/zalando/go-keyring"
)

// Kinds of store.
const (
	KindOS   = "keychain" // the operating system's credential store
	KindFile = "file"     // a file of mode 0600
)

// ErrNotFound: there is no value under the key.
var ErrNotFound = errors.New("no value is stored under that name")

// Store keeps values by key.
type Store interface {
	Get(key string) (string, error) // ErrNotFound when there is none
	Set(key, value string) error
	// Delete removes the value; none to remove is fine.
	Delete(key string) error
}

// service is what the items are filed under in the credential store: the
// name a person sees there.
const service = "bulkhead"

// OS is the credential store of the operating system: the macOS keychain,
// the Windows Credential Manager, the Secret Service on Linux. Prefix goes
// before every key, so that one configuration directory's values (and one
// environment's) are told from another's.
type OS struct{ Prefix string }

func (s OS) Get(key string) (string, error) {
	value, err := keyring.Get(service, s.Prefix+key)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	return value, err
}

func (s OS) Set(key, value string) error { return keyring.Set(service, s.Prefix+key, value) }

func (s OS) Delete(key string) error {
	err := keyring.Delete(service, s.Prefix+key)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// Probe reports whether the credential store can be used here, by putting
// a value in and taking it out again. It cannot where nobody is logged in
// at the screen (a macOS keychain is locked over ssh), and on a Linux
// without a Secret Service.
func Probe(prefix string) error {
	store := OS{Prefix: prefix}
	const key, value = "probe", "bh"
	if err := store.Set(key, value); err != nil {
		return err
	}
	got, err := store.Get(key)
	if err == nil && got != value {
		err = errors.New("the credential store returned another value than it was given")
	}
	if deleteErr := store.Delete(key); err == nil {
		err = deleteErr
	}
	return err
}

// File keeps values in one JSON file, mode 0600, written whole each time.
type File struct{ Path string }

func (s File) load() (map[string]string, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("%s: %w", s.Path, err)
	}
	return values, nil
}

func (s File) save(values map[string]string) error {
	if len(values) == 0 {
		err := os.Remove(s.Path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(values); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	temp := s.Path + ".tmp"
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
		err = os.Rename(temp, s.Path)
	}
	if err != nil {
		_ = os.Remove(temp)
	}
	return err
}

func (s File) Get(key string) (string, error) {
	values, err := s.load()
	if err != nil {
		return "", err
	}
	value, found := values[key]
	if !found {
		return "", ErrNotFound
	}
	return value, nil
}

func (s File) Set(key, value string) error {
	values, err := s.load()
	if err != nil {
		return err
	}
	values[key] = value
	return s.save(values)
}

func (s File) Delete(key string) error {
	values, err := s.load()
	if err != nil {
		return err
	}
	if _, found := values[key]; !found {
		return nil
	}
	delete(values, key)
	return s.save(values)
}

// Memory keeps values in memory: for tests.
type Memory map[string]string

func (m Memory) Get(key string) (string, error) {
	value, found := m[key]
	if !found {
		return "", ErrNotFound
	}
	return value, nil
}

func (m Memory) Set(key, value string) error { m[key] = value; return nil }
func (m Memory) Delete(key string) error     { delete(m, key); return nil }
