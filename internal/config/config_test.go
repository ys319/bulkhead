package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestDir(t *testing.T) {
	tests := []struct {
		name string
		goos string
		vars map[string]string
		want string
	}{
		{"explicit wins", "linux", map[string]string{"BH_CONFIG_DIR": "/x", "XDG_CONFIG_HOME": "/xdg", "HOME": "/h"}, "/x"},
		{"xdg", "linux", map[string]string{"XDG_CONFIG_HOME": "/xdg", "HOME": "/h"}, filepath.Join("/xdg", "bh")},
		{"home", "darwin", map[string]string{"HOME": "/h"}, filepath.Join("/h", ".config", "bh")},
		{"windows", "windows", map[string]string{"APPDATA": `C:\Users\u\AppData\Roaming`, "HOME": "/h"}, filepath.Join(`C:\Users\u\AppData\Roaming`, "bh")},
		// The TypeScript version's override does not move this version's directory.
		{"the old override is not ours", "linux", map[string]string{"BULKHEAD_CONFIG_DIR": "/old", "HOME": "/h"}, filepath.Join("/h", ".config", "bh")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Dir(tt.goos, env(tt.vars))
			if err != nil || got != tt.want {
				t.Errorf("Dir = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
	if _, err := Dir("windows", env(nil)); err == nil {
		t.Error("Windows without APPDATA should fail")
	}
	if _, err := Dir("linux", env(nil)); err == nil {
		t.Error("no HOME should fail")
	}
}

func TestParseEnvironment(t *testing.T) {
	got, err := ParseEnvironment([]byte(`{"$schema": "x", "version": 1, "name": "web", "type": "local", "sshPort": 22001}`))
	if err != nil {
		t.Fatal(err)
	}
	want := Environment{Name: "web", Type: TypeLocal, SSHPort: 22001, Restart: "unless-stopped", GPU: "off"}
	if got.Name != want.Name || got.Type != want.Type || got.SSHPort != want.SSHPort ||
		got.Restart != want.Restart || got.GPU != want.GPU || got.Publish || got.DockerSocket {
		t.Errorf("defaults: got %+v, want %+v", got, want)
	}

	remote, err := ParseEnvironment([]byte(`{"version": 1, "name": "box", "type": "remote", "host": "10.0.0.5", "sshPort": 22}`))
	if err != nil || remote.Host != "10.0.0.5" || remote.Local() {
		t.Errorf("remote: %+v, %v", remote, err)
	}
}

func TestParseEnvironmentRefuses(t *testing.T) {
	tests := []struct {
		name, json, want string
	}{
		{"a file of the TypeScript version", `{"type": "local", "sshPort": 22001}`, "TypeScript version"},
		{"a newer format", `{"version": 2, "name": "a", "type": "local", "sshPort": 1}`, "version 2"},
		{"an unknown field", `{"version": 1, "name": "a", "type": "local", "sshPort": 22001, "sshport2": 1}`, "unknown field"},
		{"content after the value", `{"version": 1, "name": "a", "type": "local", "sshPort": 22001} x`, "after the JSON value"},
		{"a missing field", `{"version": 1, "type": "local"}`, "missing: name, sshPort"},
		{"a bad name", `{"version": 1, "name": "Web", "type": "local", "sshPort": 22001}`, "lowercase"},
		{"a device name", `{"version": 1, "name": "nul", "type": "local", "sshPort": 22001}`, "Windows device"},
		{"a bad port", `{"version": 1, "name": "a", "type": "local", "sshPort": 70000}`, "sshPort"},
		{"a bad restart policy", `{"version": 1, "name": "a", "type": "local", "sshPort": 1, "restart": "always"}`, "restart"},
		{"a bad key reference", `{"version": 1, "name": "a", "type": "local", "sshPort": 1, "authorizedKeys": ["ssh-ed25519 AAAA"]}`, "fingerprint"},
		{"a bad gpu mode", `{"version": 1, "name": "a", "type": "local", "sshPort": 1, "gpu": "all"}`, "gpu"},
		{"a bad cdi device", `{"version": 1, "name": "a", "type": "local", "sshPort": 1, "cdiDevices": ["gpu0"]}`, "vendor/class=name"},
		{"an unknown type", `{"version": 1, "name": "a", "type": "cloud", "sshPort": 1}`, "type"},
		{"a host on a local environment", `{"version": 1, "name": "a", "type": "local", "sshPort": 1, "host": "h"}`, "not allowed"},
		{"local settings on a remote one", `{"version": 1, "name": "a", "type": "remote", "host": "h", "sshPort": 1, "publish": true}`, "not allowed"},
		{"a host that would break out of the ssh config", `{"version": 1, "name": "a", "type": "remote", "host": "h\n  ProxyCommand x", "sshPort": 1}`, "host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseEnvironment([]byte(tt.json))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one mentioning %q", err, tt.want)
			}
		})
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	owner, err := store.Init()
	if err != nil || !ValidID(owner) {
		t.Fatalf("Init = %q, %v", owner, err)
	}
	if again, _ := store.Init(); again != owner {
		t.Errorf("a second Init changed the owner: %q → %q", owner, again)
	}

	local := NewLocal("aaaa00000001", "web", 22001)
	local.AuthorizedKeys = []string{"SHA256:UPz/tYMFZj8joscMirdI1bpNKZGbx8PZ3zVMEvb9kZ8"}
	remote := Environment{ID: "aaaa00000002", Name: "box", Type: TypeRemote, Host: "example.com", SSHPort: 22}
	for _, env := range []Environment{local, remote} {
		if err := store.SaveEnvironment(env); err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Envs) != 2 || cfg.Envs[0].Name != "box" || cfg.Envs[1].Name != "web" {
		t.Fatalf("environments (sorted by name): %+v", cfg.Envs)
	}
	got, err := cfg.Find("web")
	if err != nil || got.ID != local.ID || got.SSHPort != 22001 || len(got.AuthorizedKeys) != 1 {
		t.Errorf("Find(web) = %+v, %v", got, err)
	}
	if _, err := cfg.Find("nope"); err == nil {
		t.Error("Find of an unknown name should fail")
	}
	// The remote port is on another machine; it does not count.
	if port := cfg.SuggestPort(); port != 22002 {
		t.Errorf("SuggestPort = %d, want 22002", port)
	}

	// Every setting is written out, so the file shows what can be edited.
	written, _ := os.ReadFile(store.EnvFile(local.ID))
	for _, field := range []string{`"version": 1`, `"publish": false`, `"restart": "unless-stopped"`, `"gpu": "off"`, `"cdiDevices": []`, `"dockerSocket": false`} {
		if !strings.Contains(string(written), field) {
			t.Errorf("env.json lacks %s:\n%s", field, written)
		}
	}
}

func TestLoadKeepsABrokenEnvironmentApart(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	if _, err := store.Init(); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveEnvironment(NewLocal("good00000001", "good", 22001)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store.EnvDir("bad000000001"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.EnvFile("bad000000001"), []byte(`{"version": 1, "name": `), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory without env.json is not an environment at all.
	if err := os.MkdirAll(store.EnvDir("empty0000001"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Envs) != 1 || len(cfg.Broken) != 1 || cfg.Broken[0].ID != "bad000000001" {
		t.Fatalf("got %+v", cfg)
	}
	// It is found by its id, with what is wrong with it.
	if _, err := cfg.Find("bad000000001"); err == nil || !strings.Contains(err.Error(), "env.json") {
		t.Errorf("Find(broken) = %v", err)
	}
}

func TestLoadRefusesConflicts(t *testing.T) {
	t.Run("two environments with one name", func(t *testing.T) {
		store := Store{Dir: t.TempDir()}
		store.Init()
		store.SaveEnvironment(NewLocal("aaaa00000001", "web", 22001))
		store.SaveEnvironment(NewLocal("aaaa00000002", "web", 22002))
		if _, err := store.Load(); err == nil || !strings.Contains(err.Error(), `named "web"`) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("two local environments on one port", func(t *testing.T) {
		store := Store{Dir: t.TempDir()}
		store.Init()
		store.SaveEnvironment(NewLocal("aaaa00000001", "a", 22001))
		store.SaveEnvironment(NewLocal("aaaa00000002", "b", 22001))
		if _, err := store.Load(); err == nil || !strings.Contains(err.Error(), "sshPort 22001") {
			t.Errorf("err = %v", err)
		}
	})
}

// A directory of the TypeScript version must not be operated on: its
// environments have other container names, and its files another format.
func TestALegacyDirectoryIsRefused(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	if err := os.MkdirAll(store.EnvDir("main"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.EnvFile("main"), []byte(`{"type": "local", "sshPort": 22001}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Load(); !errors.Is(err, ErrLegacy) {
		t.Errorf("Load: %v, want ErrLegacy", err)
	}
	if _, err := store.Init(); !errors.Is(err, ErrLegacy) {
		t.Errorf("Init: %v, want ErrLegacy", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "bulkhead.json")); err == nil {
		t.Error("Init claimed a legacy directory")
	}
}

func TestAnEmptyDirectoryIsJustEmpty(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "not-there-yet")}
	cfg, err := store.Load()
	if err != nil || len(cfg.Envs) != 0 || len(cfg.Broken) != 0 {
		t.Errorf("Load = %+v, %v", cfg, err)
	}
	if _, err := os.Stat(store.Dir); err == nil {
		t.Error("reading created the directory")
	}
}

func TestResolveKeys(t *testing.T) {
	const line = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIESD9O48y6Q1airXig+Rz77VFzZ5LlDMfbOtr2U9FH+N someone@example"
	const print = "SHA256:UPz/tYMFZj8joscMirdI1bpNKZGbx8PZ3zVMEvb9kZ8"
	const other = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

	store := Store{Dir: t.TempDir()}
	if keys, err := store.LoadKeys(); err != nil || keys != nil {
		t.Fatalf("no keys.json: %v, %v", keys, err)
	}
	registry := `{"keys": [{"fingerprint": "` + print + `", "publicKey": "` + line + `", "label": "laptop"}]}`
	if err := os.WriteFile(store.KeyRegistry(), []byte(registry), 0o644); err != nil {
		t.Fatal(err)
	}
	keys, err := store.LoadKeys()
	if err != nil {
		t.Fatal(err)
	}

	web := NewLocal("aaaa00000001", "web", 22001)
	web.AuthorizedKeys = []string{print}
	if got, err := ResolveKeys(keys, web); err != nil || len(got) != 1 || got[0] != line {
		t.Errorf("ResolveKeys = %v, %v", got, err)
	}

	web.AuthorizedKeys = []string{other}
	if _, err := ResolveKeys(keys, web); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("a dangling reference: %v", err)
	}

	// The entry was swapped for another key under the old fingerprint.
	tampered := []KeyEntry{{Fingerprint: other, PublicKey: line}}
	if _, err := ResolveKeys(tampered, web); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("a tampered entry: %v", err)
	}
}
