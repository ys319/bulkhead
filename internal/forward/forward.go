// Package forward is about ports carried between this machine and an
// environment over its ssh connection: how one is written, what the ssh
// client is told, and who has a named set of them open.
//
// Nothing here knows whether the environment runs on this machine's Docker
// or on another machine: a forward goes through the same ssh endpoint a
// shell does, so it works for both.
package forward

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Port is one forwarded port.
//
// It is written the way Docker writes a published port: the port of this
// machine left, the port of the environment right (8080:80), or one number
// for both (3000). Like there, that alone means the service is in the
// environment and this machine listens. With host: in front the service
// is on this machine and the environment listens — the direction that lets
// what runs in the environment reach something outside it, which is why
// it takes a word to ask for. The sides stay where they are: host:80:8080
// is this machine's 80, at 8080 in the environment.
type Port struct {
	Host int // on this machine
	Env  int // in the environment
	// FromHost: the service is on this machine, and the environment
	// reaches it at Env.
	FromHost bool
}

var portPattern = regexp.MustCompile(`^(host:)?([0-9]{1,5})(?::([0-9]{1,5}))?$`)

// IsPort reports whether the word is written like a port (and not like the
// name of a saved set).
func IsPort(word string) bool { return portPattern.MatchString(word) }

// Parse reads a port as it is written on the command line and in env.json.
func Parse(text string) (Port, error) {
	match := portPattern.FindStringSubmatch(text)
	if match == nil {
		return Port{}, fmt.Errorf("%q is not a port to forward: 3000, 8080:80 (this machine's port, then the environment's), or host:8000 for a port of this machine", text)
	}
	number := func(digits string) (int, error) {
		port, err := strconv.Atoi(digits)
		if err != nil || port < 1 || port > 65535 {
			return 0, fmt.Errorf("%q: a port is a number between 1 and 65535", text)
		}
		return port, nil
	}
	host, err := number(match[2])
	if err != nil {
		return Port{}, err
	}
	env := host
	if match[3] != "" {
		if env, err = number(match[3]); err != nil {
			return Port{}, err
		}
	}
	return Port{Host: host, Env: env, FromHost: match[1] != ""}, nil
}

// ParseAll reads several ports and checks them together.
func ParseAll(words []string) ([]Port, error) {
	ports := make([]Port, 0, len(words))
	for _, word := range words {
		port, err := Parse(word)
		if err != nil {
			return nil, err
		}
		ports = append(ports, port)
	}
	return ports, Check(ports)
}

// String writes the port the way Parse reads it, in its shortest form.
func (p Port) String() string {
	text := strconv.Itoa(p.Host)
	if p.Env != p.Host {
		text += ":" + strconv.Itoa(p.Env)
	}
	if p.FromHost {
		return "host:" + text
	}
	return text
}

// Describe says which way connections go, in words: where one is made, and
// where it comes out.
func (p Port) Describe() string {
	if p.FromHost {
		return fmt.Sprintf("the environment's localhost:%d → this machine's port %d", p.Env, p.Host)
	}
	return fmt.Sprintf("this machine's localhost:%d → the environment's port %d", p.Host, p.Env)
}

// Strings writes the ports.
func Strings(ports []Port) []string {
	out := make([]string, len(ports))
	for i, port := range ports {
		out[i] = port.String()
	}
	return out
}

// Check refuses ports that cannot be open together: two that listen on
// the same port of the same side.
func Check(ports []Port) error {
	type listener struct {
		inEnv bool
		port  int
	}
	seen := map[listener]Port{}
	for _, port := range ports {
		at := listener{false, port.Host}
		if port.FromHost {
			at = listener{true, port.Env}
		}
		if other, taken := seen[at]; taken {
			side := "this machine"
			if at.inEnv {
				side = "the environment"
			}
			return fmt.Errorf("%s and %s both listen on port %d of %s", other, port, at.port, side)
		}
		seen[at] = port
	}
	return nil
}

// SSHArgs are the options that make the ssh client carry the ports.
//
// Both ends listen on the loopback address only: a forward is for this
// machine and the environment, not for the networks they are on. What a
// forward from this machine leads to is this machine's localhost (by name:
// the client tries ::1 and 127.0.0.1, wherever the service listens); in
// the environment, which has no IPv6, it is 127.0.0.1.
func SSHArgs(ports []Port) []string {
	var args []string
	for _, port := range ports {
		if port.FromHost {
			args = append(args, "-R", fmt.Sprintf("127.0.0.1:%d:localhost:%d", port.Env, port.Host))
		} else {
			args = append(args, "-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", port.Host, port.Env))
		}
	}
	return args
}

// SessionOptions are the ssh options of a connection that only carries
// ports: it fails instead of going on without a port it could not listen
// on, never asks at a terminal, and notices within a minute that the other
// side is gone.
func SessionOptions() []string {
	return []string{"ExitOnForwardFailure=yes", "BatchMode=yes", "ServerAliveInterval=15", "ServerAliveCountMax=3"}
}

// A Set is ports saved under a name with an environment's settings.
type Set struct {
	Name string
	// On: it is opened by bh forward run without a name, and by the
	// full-screen interface for as long as that is open.
	On    bool
	Ports []Port
}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// CheckName checks the name of a set. A name starts with a letter, which
// is what tells it from a port wherever the two are written side by side.
func CheckName(name string) error {
	switch {
	case !namePattern.MatchString(name):
		return errors.New("the name of saved forwards uses lowercase letters, digits and hyphens, starts with a letter and has at most 32 characters")
	case name == "host":
		return errors.New(`"host" is the word for a port of this machine, not a name`)
	}
	return nil
}

// CheckSets checks an environment's sets: their names, and each one's
// ports among themselves. Two sets may want the same port — whether they
// can be open together shows when both are.
func CheckSets(sets []Set) error {
	seen := map[string]bool{}
	for _, set := range sets {
		if err := CheckName(set.Name); err != nil {
			return fmt.Errorf("%q: %w", set.Name, err)
		}
		if seen[set.Name] {
			return fmt.Errorf("%q is there twice", set.Name)
		}
		seen[set.Name] = true
		if len(set.Ports) == 0 {
			return fmt.Errorf("%q has no ports", set.Name)
		}
		if err := Check(set.Ports); err != nil {
			return fmt.Errorf("%q: %w", set.Name, err)
		}
	}
	return nil
}

// Find looks a set up by name.
func Find(sets []Set, name string) (Set, bool) {
	for _, set := range sets {
		if set.Name == name {
			return set, true
		}
	}
	return Set{}, false
}

// Summary is a set in a line: its ports as they are written.
func (s Set) Summary() string { return strings.Join(Strings(s.Ports), " ") }

// FromHost reports whether any of the ports lets the environment reach
// this machine.
func FromHost(ports []Port) bool {
	for _, port := range ports {
		if port.FromHost {
			return true
		}
	}
	return false
}
