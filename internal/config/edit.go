package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Changes are edits to a local environment's settings. A nil field stays
// as it is.
type Changes struct {
	Publish        *bool
	SSHPort        *int
	Restart        *string
	GPU            *string
	CDIDevices     *[]string
	DockerSocket   *bool
	AuthorizedKeys *[]string
}

// Locked names the changed settings that cannot change while the
// environment runs: its containers were created from them, and bh would
// then say one thing while Docker does another. Keys are not among them —
// the next start picks them up.
func (c Changes) Locked() []string {
	var locked []string
	add := func(changed bool, name string) {
		if changed {
			locked = append(locked, name)
		}
	}
	add(c.Publish != nil, "publish")
	add(c.SSHPort != nil, "sshPort")
	add(c.Restart != nil, "restart")
	add(c.GPU != nil, "gpu")
	add(c.CDIDevices != nil, "cdiDevices")
	add(c.DockerSocket != nil, "dockerSocket")
	return locked
}

// Empty reports whether nothing would change.
func (c Changes) Empty() bool {
	return len(c.Locked()) == 0 && c.AuthorizedKeys == nil
}

// With returns the environment with the changes applied, unchecked.
func (e Environment) With(c Changes) Environment {
	if c.Publish != nil {
		e.Publish = *c.Publish
	}
	if c.SSHPort != nil {
		e.SSHPort = *c.SSHPort
	}
	if c.Restart != nil {
		e.Restart = *c.Restart
	}
	if c.GPU != nil {
		e.GPU = *c.GPU
	}
	if c.CDIDevices != nil {
		e.CDIDevices = slices.Clone(*c.CDIDevices)
	}
	if c.DockerSocket != nil {
		e.DockerSocket = *c.DockerSocket
	}
	if c.AuthorizedKeys != nil {
		e.AuthorizedKeys = slices.Clone(*c.AuthorizedKeys)
	}
	return e
}

// Check is what Load checks across environments, for one that is about to
// be saved: a valid shape, a name of its own, and for a local one an ssh
// port of its own.
func (c *Config) Check(env Environment) error {
	if err := env.validate(); err != nil {
		return err
	}
	if env.Local() && env.GPU == "cdi" && len(env.CDIDevices) == 0 {
		return errors.New("gpu: cdi needs at least one device (--cdi-devices; see bh gpu list)")
	}
	for _, other := range c.Envs {
		if other.ID == env.ID {
			continue
		}
		if other.Name == env.Name {
			return fmt.Errorf("an environment named %q already exists", env.Name)
		}
		if env.Local() && other.Local() && other.SSHPort == env.SSHPort {
			return fmt.Errorf("port %d is already the ssh port of %q", env.SSHPort, other.Name)
		}
	}
	return nil
}

// NewRemote returns a pointer to an environment on another machine.
func NewRemote(id, name, host string, sshPort int) Environment {
	return Environment{ID: id, Name: name, Type: TypeRemote, Host: host, SSHPort: sshPort}
}

// RemoteAddress is how a remote environment is written down: the host, its
// ssh port there, and its name there. The port is never implied — every
// environment has its own, and a default would reach a different one.
type RemoteAddress struct {
	Host string
	Port int
	Name string
}

// RemoteAddressHint shows the form of a RemoteAddress.
const RemoteAddressHint = "host:port/name (for example studio.test:22001/main)"

var remoteAddressPattern = regexp.MustCompile(`^([^\s:/"'\x00-\x1f\x7f]+):(\d+)/(.+)$`)

// ParseRemoteAddress reads host:port/name. The host has no ":" or "/"
// (no IPv6: the holder turns IPv6 off).
func ParseRemoteAddress(input string) (RemoteAddress, error) {
	input = strings.TrimSpace(input)
	matched := remoteAddressPattern.FindStringSubmatch(input)
	if matched == nil {
		return RemoteAddress{}, fmt.Errorf("expected %s, got %q", RemoteAddressHint, input)
	}
	port, err := strconv.Atoi(matched[2])
	if err != nil || !validPort(port) {
		return RemoteAddress{}, fmt.Errorf("the port must be between 1 and 65535, got %s", matched[2])
	}
	return RemoteAddress{Host: matched[1], Port: port, Name: matched[3]}, nil
}

func (a RemoteAddress) String() string {
	return a.Host + ":" + strconv.Itoa(a.Port) + "/" + a.Name
}
