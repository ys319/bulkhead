package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Environment types.
const (
	TypeLocal  = "local"  // containers on this machine's Docker
	TypeRemote = "remote" // an environment on another machine, reached by ssh
)

// Environment is the settings of one environment (env.json).
type Environment struct {
	// ID identifies the Docker resources and the directory. It comes from
	// the directory name, not from the file, and never changes.
	ID string
	// Name is what people type and see. Unique among environments.
	Name string
	Type string

	// SSHPort is the host port for a local environment (unique among local
	// environments) and the port to connect to for a remote one.
	SSHPort int

	// Local only.
	Publish        bool     // listen on all interfaces instead of loopback
	Restart        string   // "unless-stopped" or "no", for both containers
	AuthorizedKeys []string // fingerprints of registered keys allowed in
	GPU            string   // "off", "legacy" or "cdi"
	CDIDevices     []string // devices for GPU "cdi"
	DockerSocket   bool     // opt-in that gives up the boundary for this environment
	// Archived is set while the environment is put away: the name of the
	// snapshot, in its snapshot directory, that holds its data. Its
	// containers and volumes (all but the ssh host identity) are gone
	// until bh unarchive brings them back from that file.
	Archived string

	// Remote only.
	Host string
}

// Local reports whether the environment runs on this machine's Docker.
func (e Environment) Local() bool { return e.Type == TypeLocal }

// NewLocal returns a local environment with the default settings.
func NewLocal(id, name string, sshPort int) Environment {
	return Environment{
		ID: id, Name: name, Type: TypeLocal, SSHPort: sshPort,
		Restart: "unless-stopped", GPU: "off",
	}
}

var (
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// Names of Windows devices cannot be directories there. Refused on
	// every platform so that a configuration stays portable.
	reservedDeviceName = regexp.MustCompile(`^(con|prn|aux|nul|com[1-9]|lpt[1-9])$`)
	fingerprintPattern = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
	// A fully qualified CDI device name: vendor/class=name.
	cdiPattern = regexp.MustCompile(`^[A-Za-z](?:[A-Za-z0-9_.-]*[A-Za-z0-9])?/[A-Za-z](?:[A-Za-z0-9_.-]*[A-Za-z0-9])?=[A-Za-z0-9](?:[A-Za-z0-9_.:-]*[A-Za-z0-9])?$`)
	// A snapshot is a file in the environment's snapshot directory.
	snapshotNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.tar\.gz$`)
	// The host goes into an ssh config fragment as it is: no whitespace,
	// quotes or control characters.
	hostPattern = regexp.MustCompile(`^[^\s"\x00-\x1f\x7f]+$`)
)

// ValidateName checks a name for an environment. Names become the hostname
// and the ssh alias.
func ValidateName(name string) error {
	switch {
	case !namePattern.MatchString(name):
		return errors.New("environment names use lowercase letters, digits and hyphens, and start with a letter or digit")
	case len(name) > 32:
		return errors.New("environment names are limited to 32 characters")
	case reservedDeviceName.MatchString(name):
		return errors.New("environment names cannot be a Windows device name (con, prn, aux, nul, com1-9, lpt1-9)")
	}
	return nil
}

func validPort(port int) bool { return port >= 1 && port <= 65535 }

func (e Environment) validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if err := ValidateName(e.Name); err != nil {
		add("name: %v", err)
	}
	if !validPort(e.SSHPort) {
		add("sshPort: must be between 1 and 65535")
	}
	switch e.Type {
	case TypeLocal:
		if e.Restart != "unless-stopped" && e.Restart != "no" {
			add(`restart: must be "unless-stopped" or "no"`)
		}
		for _, ref := range e.AuthorizedKeys {
			if !fingerprintPattern.MatchString(ref) {
				add("authorizedKeys: %q is not a key fingerprint (SHA256:...)", ref)
			}
		}
		if e.GPU != "off" && e.GPU != "legacy" && e.GPU != "cdi" {
			add(`gpu: must be "off", "legacy" or "cdi"`)
		}
		for _, device := range e.CDIDevices {
			if !cdiPattern.MatchString(device) {
				add("cdiDevices: %q does not look like vendor/class=name (for example nvidia.com/gpu=0)", device)
			}
		}
		if e.Archived != "" && !snapshotNamePattern.MatchString(e.Archived) {
			add("archived: %q is not the name of a snapshot file", e.Archived)
		}
	case TypeRemote:
		if !hostPattern.MatchString(e.Host) {
			add("host: takes a hostname or IP address (no spaces, quotes or control characters)")
		}
	default:
		add(`type: must be "local" or "remote"`)
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

// envFile is env.json as it is read: every field optional, so that missing
// and wrongly placed fields can be told apart and reported.
type envFile struct {
	Schema         *string   `json:"$schema"` // for editors; ignored
	Version        *int      `json:"version"`
	Name           *string   `json:"name"`
	Type           *string   `json:"type"`
	Host           *string   `json:"host"`
	SSHPort        *int      `json:"sshPort"`
	Publish        *bool     `json:"publish"`
	Restart        *string   `json:"restart"`
	AuthorizedKeys *[]string `json:"authorizedKeys"`
	GPU            *string   `json:"gpu"`
	CDIDevices     *[]string `json:"cdiDevices"`
	DockerSocket   *bool     `json:"dockerSocket"`
	Archived       *string   `json:"archived"`
}

// ParseEnvironment reads the contents of an env.json. The result has no ID:
// that is the directory's name.
func ParseEnvironment(data []byte) (Environment, error) {
	var raw envFile
	if err := decodeStrict(data, &raw); err != nil {
		return Environment{}, err
	}
	switch {
	case raw.Version == nil:
		return Environment{}, errors.New("no version: this file was written by the TypeScript version of bulkhead and has not been migrated")
	case *raw.Version != FormatVersion:
		return Environment{}, fmt.Errorf("version %d: this bh reads version %d", *raw.Version, FormatVersion)
	}
	return raw.environment()
}

// ParseLegacyEnvironment reads an env.json of the TypeScript version. That
// format has no version and no name: the name was the directory's, and
// becomes both the name and the id here.
func ParseLegacyEnvironment(data []byte, name string) (Environment, error) {
	var raw envFile
	if err := decodeStrict(data, &raw); err != nil {
		return Environment{}, err
	}
	if raw.Version != nil || raw.Name != nil {
		return Environment{}, errors.New("this file has a version or a name: it is not in the format of the TypeScript version")
	}
	raw.Name = &name
	env, err := raw.environment()
	env.ID = name
	return env, err
}

// environment checks the fields and applies the defaults.
func (raw envFile) environment() (Environment, error) {
	var missing, misplaced []string
	env := Environment{}
	if raw.Name == nil {
		missing = append(missing, "name")
	} else {
		env.Name = *raw.Name
	}
	if raw.Type == nil {
		missing = append(missing, "type")
	} else {
		env.Type = *raw.Type
	}
	if raw.SSHPort == nil {
		missing = append(missing, "sshPort")
	} else {
		env.SSHPort = *raw.SSHPort
	}

	localOnly := map[string]bool{
		"publish": raw.Publish != nil, "restart": raw.Restart != nil,
		"authorizedKeys": raw.AuthorizedKeys != nil, "gpu": raw.GPU != nil,
		"cdiDevices": raw.CDIDevices != nil, "dockerSocket": raw.DockerSocket != nil,
		"archived": raw.Archived != nil,
	}
	switch env.Type {
	case TypeLocal:
		env.Restart, env.GPU = "unless-stopped", "off"
		if raw.Publish != nil {
			env.Publish = *raw.Publish
		}
		if raw.Restart != nil {
			env.Restart = *raw.Restart
		}
		if raw.AuthorizedKeys != nil {
			env.AuthorizedKeys = *raw.AuthorizedKeys
		}
		if raw.GPU != nil {
			env.GPU = *raw.GPU
		}
		if raw.CDIDevices != nil {
			env.CDIDevices = *raw.CDIDevices
		}
		if raw.DockerSocket != nil {
			env.DockerSocket = *raw.DockerSocket
		}
		if raw.Archived != nil {
			env.Archived = *raw.Archived
		}
		if raw.Host != nil {
			misplaced = append(misplaced, "host")
		}
	case TypeRemote:
		if raw.Host == nil {
			missing = append(missing, "host")
		} else {
			env.Host = *raw.Host
		}
		for _, field := range []string{"publish", "restart", "authorizedKeys", "gpu", "cdiDevices", "dockerSocket", "archived"} {
			if localOnly[field] {
				misplaced = append(misplaced, field)
			}
		}
	}

	var problems []string
	if len(missing) > 0 {
		problems = append(problems, "missing: "+strings.Join(missing, ", "))
	}
	if len(misplaced) > 0 {
		problems = append(problems, fmt.Sprintf("not allowed for type %q: %s", env.Type, strings.Join(misplaced, ", ")))
	}
	if len(missing) == 0 {
		if err := env.validate(); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) > 0 {
		return Environment{}, errors.New(strings.Join(problems, "\n"))
	}
	return env, nil
}

type localFile struct {
	Version        int      `json:"version"`
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	SSHPort        int      `json:"sshPort"`
	Publish        bool     `json:"publish"`
	Restart        string   `json:"restart"`
	AuthorizedKeys []string `json:"authorizedKeys"`
	GPU            string   `json:"gpu"`
	CDIDevices     []string `json:"cdiDevices"`
	DockerSocket   bool     `json:"dockerSocket"`
	// Only there while the environment is archived: not a setting to edit.
	Archived string `json:"archived,omitempty"`
}

type remoteFile struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Host    string `json:"host"`
	SSHPort int    `json:"sshPort"`
}

// file is the environment as it is written: every field stated, so the file
// shows what can be edited.
func (e Environment) file() any {
	if e.Type == TypeRemote {
		return remoteFile{Version: FormatVersion, Name: e.Name, Type: e.Type, Host: e.Host, SSHPort: e.SSHPort}
	}
	return localFile{
		Version: FormatVersion, Name: e.Name, Type: e.Type, SSHPort: e.SSHPort,
		Publish: e.Publish, Restart: e.Restart,
		AuthorizedKeys: append([]string{}, e.AuthorizedKeys...),
		GPU:            e.GPU,
		CDIDevices:     append([]string{}, e.CDIDevices...),
		DockerSocket:   e.DockerSocket,
		Archived:       e.Archived,
	}
}
