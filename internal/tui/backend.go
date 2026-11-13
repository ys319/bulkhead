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
	// Tokens are the credentials handed to the environment, without their
	// values. TokenProblem says why their records could not be read.
	Tokens       []Token
	TokenProblem string
}

// Token is a credential handed to an environment.
type Token struct {
	Kind    string // "github", "hf" or "custom"
	Name    string // the variable it is exported as
	Summary string // GitHub's token in a line; empty for the others
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

	// Tokens. Each returns what to tell the user when it worked; the
	// values never come back.
	SetToken(ctx context.Context, name, kind, tokenName, value string) (string, error)
	RemoveToken(ctx context.Context, name, tokenName string) (string, error)
	// TokenPage is where a token of the kind is issued; empty when there
	// is no such page.
	TokenPage(kind string) string
	// GitHubIssue prepares issuing a GitHub token: the page that has the
	// form filled in, and the name the token gets.
	GitHubIssue(name, preset string) (address, tokenName string, err error)
	SetGitHub(ctx context.Context, name, token, preset, tokenName string) (string, error)
	RevokeGitHub(ctx context.Context, name string) (string, error)
	OpenURL(address string) error

	// Snapshots: the data of a stopped environment in one file. Snapshot
	// returns the name of the one it took; Rollback the name of the one it
	// took of the data that was there (see bh rollback).
	Snapshot(ctx context.Context, name string, progress io.Writer) (string, error)
	Snapshots(name string) ([]SnapshotEntry, error)
	Rollback(ctx context.Context, name, snapshot string, progress io.Writer) (string, error)
	Archive(ctx context.Context, name string, progress io.Writer) error
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
