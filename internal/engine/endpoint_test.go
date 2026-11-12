package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeContext lays out a context the way `docker context create` does
// (layout captured from docker CLI 29.8.1).
func writeContext(t *testing.T, configDir, name, host string) {
	t.Helper()
	digest := sha256.Sum256([]byte(name))
	dir := filepath.Join(configDir, "contexts", "meta", hex.EncodeToString(digest[:]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"Name":"` + name + `","Metadata":{},"Endpoints":{"docker":{"Host":"` + host + `","SkipTLSVerify":false}}}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeConfig(t *testing.T, configDir, current string) {
	t.Helper()
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"auths":{},"currentContext":"` + current + `"}`
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func env(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

func TestResolveEndpoint(t *testing.T) {
	const desktop = "unix:///home/dev/.docker/desktop/docker.sock"

	tests := []struct {
		name    string
		env     map[string]string
		current string // currentContext in config.json ("" = no config.json)
		want    Endpoint
		wantErr string
	}{
		{
			name: "nothing set falls back to the platform default",
			want: Endpoint{Source: SourceDefault},
		},
		{
			name:    "currentContext is used",
			current: "desktop-linux",
			want:    Endpoint{Source: SourceContext, Context: "desktop-linux", Host: desktop},
		},
		{
			name:    "currentContext default is the platform default",
			current: "default",
			want:    Endpoint{Source: SourceDefault},
		},
		{
			name:    "DOCKER_CONTEXT overrides currentContext",
			env:     map[string]string{"DOCKER_CONTEXT": "other"},
			current: "desktop-linux",
			want:    Endpoint{Source: SourceContext, Context: "other", Host: "npipe:////./pipe/other"},
		},
		{
			name:    "DOCKER_CONTEXT=default overrides currentContext",
			env:     map[string]string{"DOCKER_CONTEXT": "default"},
			current: "desktop-linux",
			want:    Endpoint{Source: SourceDefault},
		},
		{
			name:    "DOCKER_HOST wins over DOCKER_CONTEXT and currentContext",
			env:     map[string]string{"DOCKER_HOST": "tcp://127.0.0.1:1", "DOCKER_CONTEXT": "other"},
			current: "desktop-linux",
			want:    Endpoint{Source: SourceHostEnv},
		},
		{
			name:    "DOCKER_HOST wins even when the named context does not exist",
			env:     map[string]string{"DOCKER_HOST": "tcp://127.0.0.1:1", "DOCKER_CONTEXT": "nosuch"},
			want:    Endpoint{Source: SourceHostEnv},
			current: "",
		},
		{
			name:    "unknown context is an error",
			env:     map[string]string{"DOCKER_CONTEXT": "nosuch"},
			wantErr: `docker context "nosuch" not found`,
		},
		{
			name:    "remote endpoints are refused, not half-configured",
			env:     map[string]string{"DOCKER_CONTEXT": "remote"},
			wantErr: "only unix:// and npipe:// endpoints are supported",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			configDir := filepath.Join(home, ".docker")
			writeContext(t, configDir, "desktop-linux", desktop)
			writeContext(t, configDir, "other", "npipe:////./pipe/other")
			writeContext(t, configDir, "remote", "ssh://dev@host.test")
			if tt.current != "" {
				writeConfig(t, configDir, tt.current)
			}

			got, err := ResolveEndpoint(env(tt.env), home)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestResolveEndpointHonorsDockerConfig(t *testing.T) {
	home := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "cfg")
	writeContext(t, elsewhere, "probe", "unix:///var/run/docker.sock")
	writeConfig(t, elsewhere, "probe")

	got, err := ResolveEndpoint(env(map[string]string{"DOCKER_CONFIG": elsewhere}), home)
	if err != nil {
		t.Fatal(err)
	}
	want := Endpoint{Source: SourceContext, Context: "probe", Host: "unix:///var/run/docker.sock"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestResolveEndpointRejectsMismatchedMetadata(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, ".docker")
	// A meta.json sitting in the directory for "wanted" but naming another context.
	digest := sha256.Sum256([]byte("wanted"))
	dir := filepath.Join(configDir, "contexts", "meta", hex.EncodeToString(digest[:]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"Name":"someone-else","Endpoints":{"docker":{"Host":"unix:///tmp/x.sock"}}}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := ResolveEndpoint(env(map[string]string{"DOCKER_CONTEXT": "wanted"}), home)
	if err == nil || !strings.Contains(err.Error(), `metadata belongs to "someone-else"`) {
		t.Fatalf("error = %v, want a metadata mismatch", err)
	}
}
