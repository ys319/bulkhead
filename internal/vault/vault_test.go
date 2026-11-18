package vault

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/ys319/bulkhead/internal/filemode"
)

func exercise(t *testing.T, store Store) {
	t.Helper()
	if _, err := store.Get("token/github"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an absent value: %v", err)
	}
	if err := store.Delete("token/github"); err != nil {
		t.Errorf("deleting an absent value: %v", err)
	}
	for key, value := range map[string]string{"token/github": "github_pat_x", "secret/API_KEY": "it's a \"value\" with spaces — ünïcode"} {
		if err := store.Set(key, value); err != nil {
			t.Fatal(err)
		}
		if got, err := store.Get(key); err != nil || got != value {
			t.Errorf("%s = %q, %v", key, got, err)
		}
	}
	if err := store.Set("token/github", "github_pat_y"); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get("token/github"); got != "github_pat_y" {
		t.Errorf("after a second set: %q", got)
	}
	if err := store.Delete("token/github"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("token/github"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
	if got, err := store.Get("secret/API_KEY"); err != nil || !strings.HasPrefix(got, "it's") {
		t.Errorf("the other value: %q, %v", got, err)
	}
}

func TestFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env", "values.json")
	store := File{Path: path}
	exercise(t, store)
	info, err := os.Stat(path)
	if err != nil || (filemode.Kept && info.Mode().Perm() != 0o600) {
		t.Fatalf("the file: %v %v", info, err)
	}
	// The last value takes the file with it: no empty file of secrets.
	if err := store.Delete("secret/API_KEY"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the empty file stayed: %v", err)
	}
}

func TestMemory(t *testing.T) { exercise(t, Memory{}) }

// The credential store itself is the operating system's; here its place is
// taken by the library's in-memory one.
func TestOSKeepsValuesApartByPrefix(t *testing.T) {
	keyring.MockInit()
	exercise(t, OS{Prefix: "owner-a/env-1/"})
	one, other := OS{Prefix: "owner-a/env-1/"}, OS{Prefix: "owner-a/env-2/"}
	if err := one.Set("token/github", "for env-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Get("token/github"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another environment's value: %v", err)
	}
	if err := Probe("owner-a/"); err != nil {
		t.Errorf("probe: %v", err)
	}
	if _, err := (OS{Prefix: "owner-a/"}).Get("probe"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the probe left its value behind: %v", err)
	}

	keyring.MockInitWithError(errors.New("User interaction is not allowed"))
	if err := Probe("owner-a/"); err == nil {
		t.Error("a store that cannot be used passed the probe")
	}
}
