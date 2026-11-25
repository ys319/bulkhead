package config

import (
	"os"
	"strings"
	"testing"
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
