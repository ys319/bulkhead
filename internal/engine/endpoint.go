// Package engine is the only place that talks to the Docker Engine.
package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Source says where the engine endpoint came from.
type Source string

const (
	SourceHostEnv Source = "DOCKER_HOST"
	SourceContext Source = "context"
	SourceDefault Source = "default"
)

// Endpoint is the resolved engine address. Host is empty for SourceHostEnv
// and SourceDefault: the client library reads the environment (and its TLS
// settings) or falls back to the platform default on its own.
type Endpoint struct {
	Source  Source
	Context string // context name, set for SourceContext
	Host    string
}

const defaultContext = "default"

// ResolveEndpoint picks the engine the docker CLI would pick. The official
// client library only reads DOCKER_HOST; contexts (how Docker Desktop and
// other runtimes publish their socket) are a CLI feature, so bh resolves them.
//
// Order, measured against docker CLI 29.8.1 on 2026-11-12:
// DOCKER_HOST > DOCKER_CONTEXT > currentContext in config.json > default.
//
// getenv is injected for tests; home is the user's home directory.
func ResolveEndpoint(getenv func(string) string, home string) (Endpoint, error) {
	if getenv("DOCKER_HOST") != "" {
		return Endpoint{Source: SourceHostEnv}, nil
	}

	configDir := getenv("DOCKER_CONFIG")
	if configDir == "" {
		configDir = filepath.Join(home, ".docker")
	}

	name := getenv("DOCKER_CONTEXT")
	if name == "" {
		current, err := currentContext(configDir)
		if err != nil {
			return Endpoint{}, err
		}
		name = current
	}
	if name == "" || name == defaultContext {
		return Endpoint{Source: SourceDefault}, nil
	}

	host, err := contextHost(configDir, name)
	if err != nil {
		return Endpoint{}, err
	}
	return Endpoint{Source: SourceContext, Context: name, Host: host}, nil
}

// currentContext reads currentContext from <configDir>/config.json. A missing
// file means the default context.
func currentContext(configDir string) (string, error) {
	path := filepath.Join(configDir, "config.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the docker config: %w", err)
	}
	var config struct {
		CurrentContext string `json:"currentContext"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return "", fmt.Errorf("parsing %s: %w", path, err)
	}
	return config.CurrentContext, nil
}

// contextHost reads the docker endpoint of a named context from the CLI's
// context store: <configDir>/contexts/meta/<sha256(name)>/meta.json.
func contextHost(configDir, name string) (string, error) {
	digest := sha256.Sum256([]byte(name))
	path := filepath.Join(configDir, "contexts", "meta", hex.EncodeToString(digest[:]), "meta.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("docker context %q not found", name)
	}
	if err != nil {
		return "", fmt.Errorf("reading docker context %q: %w", name, err)
	}
	var meta struct {
		Name      string `json:"Name"`
		Endpoints map[string]struct {
			Host string `json:"Host"`
		} `json:"Endpoints"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return "", fmt.Errorf("parsing docker context %q: %w", name, err)
	}
	// The directory name is only a hash; make sure the file is the one asked for.
	if meta.Name != name {
		return "", fmt.Errorf("docker context %q: metadata belongs to %q", name, meta.Name)
	}
	host := meta.Endpoints["docker"].Host
	if host == "" {
		return "", fmt.Errorf("docker context %q has no docker endpoint", name)
	}
	// Local sockets only for now. TLS material and ssh:// need the CLI's
	// connection helpers; fail clearly instead of connecting half-configured.
	if !strings.HasPrefix(host, "unix://") && !strings.HasPrefix(host, "npipe://") {
		return "", fmt.Errorf("docker context %q points at %s: only unix:// and npipe:// endpoints are supported (set DOCKER_HOST to override)", name, host)
	}
	return host, nil
}
