// Package tui is the full-screen interface of bh: a list of everything on
// the left, the details of the selected row on the right, and menus that
// open over them. It decides what is shown and asked; everything it does is
// done by the Backend, which is the same code the commands use.
package tui

import (
	"context"
	"io"
	"os/exec"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/ops"
)

// EnvState is an environment's settings with what the engine says about it.
type EnvState struct {
	Env config.Environment
	// Status is the state of a local environment's containers. Known is
	// false when the engine could not be asked.
	Status ops.Status
	Known  bool
}

// Snapshot is everything the list shows, read at one moment.
type Snapshot struct {
	Envs   []EnvState
	Broken []config.Broken
	Keys   []config.KeyEntry
	// EngineProblem says why the engine could not be asked, if it could not.
	EngineProblem string
}

// Address is a way other machines reach this one.
type Address struct {
	Host  string
	Label string // where it comes from: "hostname", an interface's name
}

// Backend does what the interface asks for.
type Backend interface {
	Load(ctx context.Context) (Snapshot, error)
	HasVSCode() bool

	Create(name, from string) error
	Rename(name, newName string) error
	Up(ctx context.Context, name string, progress io.Writer) error
	Down(ctx context.Context, name string) error
	Restart(ctx context.Context, name string, progress io.Writer) error
	Reset(ctx context.Context, name string) error
	Destroy(ctx context.Context, name string) error
	Configure(ctx context.Context, name string, changes config.Changes) error
	RunInit(ctx context.Context, name string, progress io.Writer) error
	Open(ctx context.Context, name string) error

	AddRemote(name, host string, port int) error
	ConfigureRemote(name string, host *string, port *int) error
	RemoveRemote(name string) error
	// Reach tries to log in to the environment; nil means it worked.
	Reach(ctx context.Context, name string) error

	RegisterKey(input, label string) (config.KeyEntry, error)
	RenameKey(fingerprint, label string) error
	RemoveKey(fingerprint string) (detachedFrom []string, err error)
	AttachKey(name, fingerprint string) error
	DetachKey(name, fingerprint string) error
	MachineKey() (string, error)

	// CDIDevices are the GPU devices the engine offers; known is false when
	// it does not say.
	CDIDevices(ctx context.Context) (devices []string, known bool, err error)
	Addresses() []Address

	// Programs that take the terminal over until they end.
	Shell(name string) (*exec.Cmd, error)        // an ssh session
	Self(args ...string) (*exec.Cmd, error)      // this bh with other arguments
	Editor(path string) (*exec.Cmd, error)       // the user's editor
	InitScript() (string, error)                 // its path, made if missing
	DevDockerfile(name string) (string, error)   // its path, made from the template if missing
	TidyDevDockerfile(name string) (bool, error) // removes a template left unedited

	// Watch reports changes of the containers: a value for every change,
	// false when the connection to the engine is lost and true when it is
	// back. The channel closes when ctx ends.
	Watch(ctx context.Context) <-chan bool
	// DiskUsage is the size of every volume, by name. It is slow enough to
	// be asked for apart from the list.
	DiskUsage(ctx context.Context) (map[string]int64, error)
	// FollowLogs writes the environment's output, the last tail lines and
	// then what follows, until ctx ends.
	FollowLogs(ctx context.Context, name string, tail int, out io.Writer) error
}
