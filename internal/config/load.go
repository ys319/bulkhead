package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
)

// Broken is an environment whose env.json could not be read. It is listed
// rather than dropped: its Docker resources and data still exist under ID.
type Broken struct {
	ID      string
	Problem string
}

// Config is everything in the directory: the environments that could be
// read, sorted by name, and the ones that could not.
type Config struct {
	Envs   []Environment
	Broken []Broken
}

// Load reads every environment. One unreadable env.json does not stop the
// others (it lands in Broken); conflicts between readable environments —
// two with the same name, two local ones on the same port — are errors,
// since no command could act on either safely.
func (s Store) Load() (*Config, error) {
	if _, err := s.Owner(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.environmentsDir())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	config := &Config{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		data, err := os.ReadFile(s.EnvFile(id))
		if errors.Is(err, fs.ErrNotExist) {
			continue // a directory without env.json is not an environment
		}
		if err != nil {
			return nil, err
		}
		if !ValidID(id) {
			config.Broken = append(config.Broken, Broken{ID: id, Problem: fmt.Sprintf("%q is not a valid environment directory name", id)})
			continue
		}
		env, err := ParseEnvironment(data)
		if err != nil {
			config.Broken = append(config.Broken, Broken{ID: id, Problem: fmt.Sprintf("%s:\n%v", s.EnvFile(id), err)})
			continue
		}
		env.ID = id
		config.Envs = append(config.Envs, env)
	}
	slices.SortFunc(config.Envs, func(a, b Environment) int { return strings.Compare(a.Name, b.Name) })

	names, ports := map[string]string{}, map[int]string{}
	for _, env := range config.Envs {
		if other, taken := names[env.Name]; taken {
			return nil, fmt.Errorf("two environments are named %q: %s and %s", env.Name, s.EnvFile(other), s.EnvFile(env.ID))
		}
		names[env.Name] = env.ID
		if !env.Local() {
			continue // a remote port is on another machine
		}
		if other, taken := ports[env.SSHPort]; taken {
			return nil, fmt.Errorf("sshPort %d is used by two local environments: %q and %q", env.SSHPort, other, env.Name)
		}
		ports[env.SSHPort] = env.Name
	}
	return config, nil
}

// Find looks an environment up by name. A broken environment is found by
// its id and reported with what is wrong with it.
func (c *Config) Find(ref string) (Environment, error) {
	for _, env := range c.Envs {
		if env.Name == ref {
			return env, nil
		}
	}
	for _, broken := range c.Broken {
		if broken.ID == ref {
			return Environment{}, fmt.Errorf("%s\n(fix the file and retry)", broken.Problem)
		}
	}
	return Environment{}, fmt.Errorf("there is no environment named %q", ref)
}

// BasePort is where ssh ports for new local environments start: reads as
// "ssh (22) + a counter", and stays clear of 2222 (Vagrant), 22000
// (Syncthing) and the ephemeral ranges.
const BasePort = 22001

// SuggestPort returns the lowest port from BasePort that no local
// environment uses.
func (c *Config) SuggestPort() int {
	used := map[int]bool{}
	for _, env := range c.Envs {
		if env.Local() {
			used[env.SSHPort] = true
		}
	}
	port := BasePort
	for used[port] {
		port++
	}
	return port
}
