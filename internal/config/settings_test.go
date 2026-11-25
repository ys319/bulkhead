package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ys319/bulkhead/internal/forward"
)

func TestSizesAreWrittenTheWayTheyAreRead(t *testing.T) {
	for text, want := range map[string]int64{
		"64m": 64 << 20, "512m": 512 << 20, "8g": 8 << 30, "16G": 16 << 30, " 4g ": 4 << 30, "8gb": 8 << 30, "1024m": 1 << 30,
	} {
		got, err := ParseSize(text)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", text, got, err, want)
		}
	}
	for _, text := range []string{"", "8", "g", "8k", "8t", "-8g", "1.5g", "8 g", "8gib", "1234567g"} {
		if _, err := ParseSize(text); err == nil {
			t.Errorf("ParseSize(%q) passed", text)
		}
	}
	for bytes, want := range map[int64]string{8 << 30: "8g", 512 << 20: "512m", 1 << 30: "1g", 1536 << 20: "1536m"} {
		if got := FormatSize(bytes); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", bytes, got, want)
		}
	}
	if _, err := ParseShmSize("32m"); err == nil || !strings.Contains(err.Error(), "at least 64m") {
		t.Errorf("a /dev/shm smaller than the engine's own: %v", err)
	}
}

func TestSettingsAreTheDefaultsUntilOneIsSet(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	settings, err := store.LoadSettings()
	if err != nil || settings != DefaultSettings() || settings.ShmSize != 8<<30 {
		t.Fatalf("no file: %+v, %v", settings, err)
	}
	if _, err := os.Stat(store.SettingsFile()); err == nil {
		t.Error("reading the settings wrote the file")
	}

	settings.ShmSize = 16 << 30
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	// Every setting is stated, in the form a person types.
	data, _ := os.ReadFile(store.SettingsFile())
	if string(data) != "{\n  \"version\": 1,\n  \"shmSize\": \"16g\"\n}\n" {
		t.Errorf("settings.json:\n%s", data)
	}
	if again, err := store.LoadSettings(); err != nil || again != settings {
		t.Errorf("round trip: %+v, %v", again, err)
	}
	// Saving initialized the directory: it is a configuration directory now.
	if owner, err := store.Owner(); err != nil || owner == "" {
		t.Errorf("owner = %q, %v", owner, err)
	}

	if err := store.SaveSettings(Settings{ShmSize: 1 << 20}); err == nil {
		t.Error("1m was saved")
	}
}

func TestSettingsThatCannotBeReadAreRefused(t *testing.T) {
	for name, content := range map[string]string{
		"a typo in a field":  `{"version": 1, "shmsize ": "8g"}`,
		"no version":         `{"shmSize": "8g"}`,
		"another version":    `{"version": 2, "shmSize": "8g"}`,
		"a number":           `{"version": 1, "shmSize": 8}`,
		"no unit":            `{"version": 1, "shmSize": "8"}`,
		"too small":          `{"version": 1, "shmSize": "8m"}`,
		"not JSON":           `shmSize = 8g`,
		"something after it": `{"version": 1} {}`,
	} {
		store := Store{Dir: t.TempDir()}
		if err := os.WriteFile(store.SettingsFile(), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		// The error names the file: that is where it is fixed.
		if _, err := store.LoadSettings(); err == nil || !strings.Contains(err.Error(), store.SettingsFile()) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// A setting that is not stated has its default.
	store := Store{Dir: t.TempDir()}
	if err := os.WriteFile(store.SettingsFile(), []byte(`{"version": 1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if settings, err := store.LoadSettings(); err != nil || settings != DefaultSettings() {
		t.Errorf("only a version: %+v, %v", settings, err)
	}
}

// Saved forwards are part of the settings of a local and of a remote
// environment alike, written the way they are typed.
func TestSavedForwardsAreKeptWithBothKindsOfEnvironment(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	if _, err := store.Init(); err != nil {
		t.Fatal(err)
	}
	sets := []forward.Set{
		{Name: "api", On: true, Ports: []forward.Port{{Host: 8000, Env: 8000, FromHost: true}}},
		{Name: "app", Ports: []forward.Port{{Host: 8080, Env: 80}, {Host: 3000, Env: 3000}}},
	}
	local := NewLocal("aaaaaaaaaaaa", "web", 22001)
	remote := NewRemote("bbbbbbbbbbbb", "box", "studio.test", 22001)
	local.Forwards, remote.Forwards = sets, sets
	for _, env := range []Environment{local, remote} {
		if err := store.SaveEnvironment(env); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(store.EnvFile(env.ID))
		if !strings.Contains(string(data), `"host:8000"`) || !strings.Contains(string(data), `"8080:80"`) {
			t.Errorf("%s env.json:\n%s", env.Name, data)
		}
	}
	cfg, err := store.Load()
	if err != nil || len(cfg.Broken) != 0 {
		t.Fatalf("load: %v, broken %v", err, cfg.Broken)
	}
	for _, env := range cfg.Envs {
		if !reflect.DeepEqual(env.Forwards, sets) {
			t.Errorf("%s: %+v", env.Name, env.Forwards)
		}
	}
	// An environment without any writes no field.
	plain := NewLocal("cccccccccccc", "plain", 22002)
	if err := store.SaveEnvironment(plain); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(store.EnvFile(plain.ID)); strings.Contains(string(data), "forwards") {
		t.Errorf("env.json of an environment without forwards:\n%s", data)
	}

	for name, forwards := range map[string]string{
		"a port that is none": `[{"name": "a", "on": true, "ports": ["web"]}]`,
		"a name that is none": `[{"name": "3000", "on": true, "ports": ["3000"]}]`,
		"no ports":            `[{"name": "a", "on": true, "ports": []}]`,
		"a name twice":        `[{"name": "a", "on": true, "ports": ["1"]}, {"name": "a", "on": true, "ports": ["2"]}]`,
		"an unknown field":    `[{"name": "a", "on": true, "ports": ["1"], "port": 1}]`,
	} {
		file := `{"version": 1, "name": "x", "type": "remote", "host": "h", "sshPort": 1, "forwards": ` + forwards + `}`
		if _, err := ParseEnvironment([]byte(file)); err == nil {
			t.Errorf("%s passed", name)
		}
	}
	// Removing the environment takes the locks of its forwards along.
	lock := store.ForwardLock(local.ID, "api")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveEnvironment(local.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(lock)); err == nil {
		t.Error("the locks outlived the environment")
	}
}
