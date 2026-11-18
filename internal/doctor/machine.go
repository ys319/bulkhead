package doctor

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/ys319/bulkhead/internal/engine"
)

// File is a file of the configuration as it was found.
type File struct {
	Path string
	Mode fs.FileMode
	// NoPermissions: the mode's permission bits say nothing about who may
	// read the file (Windows — filemode.Kept).
	NoPermissions bool
	// Err is why it could not be looked at; fs.ErrNotExist when it is not there.
	Err error
}

// Machine is what the checks of this machine's setup look at.
type Machine struct {
	// Engine is the Docker Engine's answer; nil when it gave none, and
	// EngineErr says why.
	Engine    *engine.Info
	EngineErr error

	// Programs in PATH.
	DockerCLI bool
	SSH       bool
	VSCode    bool

	MachineKey File
	InitScript File

	// Keys is how many keys are registered; KeysErr is why the registry
	// cannot be read.
	Keys    int
	KeysErr error
	// SecretStore is where the values of secrets and tokens are kept:
	// "keychain", "file", or "" while nothing was ever set.
	SecretStore string

	// Environments is how many there are, the broken ones included.
	Environments int
	// SSHConfig is the path of the user's ssh client configuration.
	SSHConfig string
	// SSHConfigErr is why it (or the file of Host entries) cannot be read.
	SSHConfigErr error
	// Included: the configuration has the Include block. IncludeBroken:
	// its markers are damaged. EntriesCurrent: the file of Host entries is
	// what bh would write now.
	Included       bool
	IncludeBroken  bool
	EntriesCurrent bool
}

// CheckMachine checks the setup every environment depends on.
func CheckMachine(m Machine) []Finding {
	var out []Finding
	out = append(out, checkEngine(m), checkDockerCLI(m), checkSSHClient(m), checkVSCode(m))
	out = append(out, checkMachineKey(m), checkKeyRegistry(m), checkSecretStore(m), checkInitScript(m))
	return append(out, checkSSHConfig(m)...)
}

func checkEngine(m Machine) Finding {
	if m.Engine == nil {
		reason := "no answer"
		if m.EngineErr != nil {
			reason = m.EngineErr.Error()
		}
		return fail("the Docker Engine does not answer: " + reason)
	}
	return ok(fmt.Sprintf("Docker Engine %s (API %s, %s/%s)", m.Engine.Version, m.Engine.APIVersion, m.Engine.OS, m.Engine.Arch))
}

// The docker CLI is needed for one thing: building the images. An
// environment whose images exist starts without it, hence a warning.
func checkDockerCLI(m Machine) Finding {
	if m.DockerCLI {
		return ok("docker is in PATH (bh builds its images with it)")
	}
	return warn("docker is not in PATH — bh cannot build images: the first bh up, and the first after bh is updated, will fail")
}

func checkSSHClient(m Machine) Finding {
	if m.SSH {
		return ok("ssh is in PATH")
	}
	return fail("ssh is not in PATH — bh ssh cannot reach any environment")
}

func checkVSCode(m Machine) Finding {
	if m.VSCode {
		return ok("code is in PATH")
	}
	return warn("code is not in PATH — bh open cannot start VS Code from here")
}

// The machine key opens every environment of this machine. The ssh client
// refuses a private key that others can read, and it is right to.
func checkMachineKey(m Machine) Finding {
	key := m.MachineKey
	switch {
	case errors.Is(key.Err, fs.ErrNotExist):
		return warn("no machine key yet (the first bh up or bh key show makes it)")
	case key.Err != nil:
		return fail(fmt.Sprintf("the machine key cannot be looked at: %v", key.Err))
	case key.NoPermissions:
		// No mode bits to check there.
		return ok("machine key is in place")
	case key.Mode.Perm()&0o077 != 0:
		return fail(fmt.Sprintf("the machine key is accessible to other users (%04o) — chmod 600 %s", key.Mode.Perm(), key.Path))
	}
	return ok("machine key is private (0600)")
}

func checkKeyRegistry(m Machine) Finding {
	switch {
	case m.KeysErr != nil:
		return fail("the key registry cannot be read — " + m.KeysErr.Error())
	case m.Keys == 0:
		return ok("key registry: no keys besides the machine key")
	}
	return ok(fmt.Sprintf("key registry: %d key(s)", m.Keys))
}

// Where values are kept is the user's choice either way; the finding says
// which it is, so that a file is not a surprise.
func checkSecretStore(m Machine) Finding {
	switch m.SecretStore {
	case "keychain":
		return ok("secrets and tokens are kept in the credential store of the operating system")
	case "file":
		return ok("secrets and tokens are kept in files readable only by you (bh secret store keychain moves them to the credential store)")
	}
	return ok("no secret or token was set yet — the first goes to the credential store, if this machine has one")
}

// init.sh is bound into every dev container. A missing file makes the
// engine refuse to create dev; one the container's user cannot read stops
// the first boot.
func checkInitScript(m Machine) Finding {
	script := m.InitScript
	switch {
	case errors.Is(script.Err, fs.ErrNotExist):
		return warn("no init.sh yet (the first bh up makes it)")
	case script.Err != nil:
		return fail(fmt.Sprintf("init.sh cannot be looked at: %v", script.Err))
	case !script.Mode.IsRegular():
		return fail(fmt.Sprintf("%s is not a file — dev containers mount it as their init script", script.Path))
	case !script.NoPermissions && script.Mode.Perm()&0o004 == 0:
		// The developer user in the container is another uid on some hosts.
		return warn(fmt.Sprintf("init.sh is not readable by others (%04o): the user in a container may not be able to run it — chmod o+r %s", script.Mode.Perm(), script.Path))
	}
	return ok("init.sh is in place")
}

// The ssh client's configuration is how `ssh bulkhead-<name>` and VS Code
// find an environment. bh rewrites it on create / up; this says whether
// that worked.
func checkSSHConfig(m Machine) []Finding {
	if m.Environments == 0 {
		return []Finding{skip("no environments — the ssh configuration is not checked")}
	}
	if m.SSHConfigErr != nil {
		return []Finding{fail(fmt.Sprintf("the ssh configuration cannot be read: %v", m.SSHConfigErr))}
	}
	var out []Finding
	switch {
	case m.IncludeBroken:
		out = append(out, fail(fmt.Sprintf("the bh markers in %s are broken — repair or remove that block by hand", m.SSHConfig)))
	case m.Included:
		out = append(out, ok("ssh configuration includes bh's Host entries"))
	default:
		out = append(out, warn("ssh configuration does not include bh's Host entries yet (the next bh create / up adds the line)"))
	}
	if m.EntriesCurrent {
		return append(out, ok("the Host entries match the environments"))
	}
	return append(out, warn("the Host entries are out of date (the next bh create / up rewrites them)"))
}
