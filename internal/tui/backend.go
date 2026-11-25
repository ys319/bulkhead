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
	"github.com/ys319/bulkhead/internal/doctor"
	"github.com/ys319/bulkhead/internal/ops"
)

// SnapshotEntry is one snapshot of an environment, newest first in a list.
type SnapshotEntry struct {
	Name string // what Rollback takes
	// About says when, how large and why, in a line.
	About string
}

// EnvState is an environment's settings with what the engine says about it.
type EnvState struct {
	Env config.Environment
	// Status is the state of a local environment's containers. Known is
	// false when the engine could not be asked.
	Status ops.Status
	Known  bool
	// Findings are what bh doctor finds about the environment now.
	Findings []doctor.Finding
	// Tokens are the credentials services issued for the environment and
	// Secrets the names of its secrets, both without values. TokenProblem
	// says why their records could not be read.
	Tokens       []Token
	Secrets      []string
	TokenProblem string
}

// Token is a credential handed to an environment.
type Token struct {
	Service string // "github", "huggingface"
	Summary string // the token in a line: access, owner, expiry
	// Urgency is "expired", "soon" or "".
	Urgency string
}

// TokenService is a service a token can be had from.
type TokenService struct {
	Name  string // what the backend takes
	Title string // what it is called on screen
	// Presets are what a token may be asked for, each including the one
	// before; none for a service whose page asks for itself.
	Presets []TokenPreset
}

// TokenPreset is one of them.
type TokenPreset struct{ Name, About string }

// TokenIssue is where a token is made and what to do there.
type TokenIssue struct {
	Address string
	Steps   []string
	// TokenName is the name the token gets on the page, where bh decides it.
	TokenName string
}

// Snapshot is everything the list shows, read at one moment.
type Snapshot struct {
	Envs   []EnvState
	Broken []config.Broken
	Keys   []config.KeyEntry
	// ShmSize is how much /dev/shm holds in an environment, as it is
	// written (8g): a setting of the machine. SettingsProblem says why the
	// settings could not be read.
	ShmSize         string
	SettingsProblem string
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
	// HasClaude: the Claude desktop app is there to hand an environment to.
	HasClaude() bool

	Create(name, from string) error
	Rename(name, newName string) error
	Up(ctx context.Context, name string, progress io.Writer) error
	Down(ctx context.Context, name string) error
	Restart(ctx context.Context, name string, progress io.Writer) error
	// Reset takes the tools back to the image, or with home the home
	// directory too (kept in a snapshot first). It returns what to tell
	// the user when it worked.
	Reset(ctx context.Context, name string, home bool, progress io.Writer) (string, error)
	Destroy(ctx context.Context, name string) error
	Configure(ctx context.Context, name string, changes config.Changes) error
	// SetShmSize changes how much /dev/shm holds in every environment
	// (512m, 16g). An environment has it once its dev container is created
	// again.
	SetShmSize(size string) error
	RunInit(ctx context.Context, name string, progress io.Writer) error
	Open(ctx context.Context, name string) error // in VS Code
	// OpenInClaude hands the environment to the Claude desktop app, which
	// asks before it connects.
	OpenInClaude(ctx context.Context, name string) error

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

	// What the environment is handed: variables, secrets, tokens. Each
	// returns what to tell the user when it worked; the values of secrets
	// and tokens never come back.
	SetVar(ctx context.Context, name, key, value string) (string, error)
	UnsetVar(ctx context.Context, name, key string) (string, error)
	SetSecret(ctx context.Context, name, key, value string) (string, error)
	RemoveSecret(ctx context.Context, name, key string) (string, error)
	TokenServices() []TokenService
	// TokenIssue prepares getting a token from the service: with refresh,
	// the one the environment has, regenerated on the service's page.
	TokenIssue(name, service, preset string, refresh bool) (TokenIssue, error)
	// SetToken checks the pasted value with the service and hands the
	// token to the environment.
	SetToken(ctx context.Context, name, service, preset, value, tokenName string, refresh bool) (string, error)
	// RemoveToken revokes the token and takes it out.
	RemoveToken(ctx context.Context, name, service string) (string, error)
	OpenURL(address string) error

	// Snapshots: the data of a stopped environment in one file. Snapshot
	// returns the name of the one it took; Rollback the name of the one it
	// took of the data that was there (see bh rollback).
	Snapshot(ctx context.Context, name string, progress io.Writer) (string, error)
	Snapshots(name string) ([]SnapshotEntry, error)
	Rollback(ctx context.Context, name, snapshot string, progress io.Writer) (string, error)
	// Archive returns what to tell the user when it worked: that, and
	// what was left to do by hand.
	Archive(ctx context.Context, name string, progress io.Writer) (string, error)
	Unarchive(ctx context.Context, name string, progress io.Writer) error

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
