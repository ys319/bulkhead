// Command bh manages bulkhead environments.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/engine"
	"github.com/ys319/bulkhead/internal/image"
	"github.com/ys319/bulkhead/internal/ops"
	"github.com/ys319/bulkhead/internal/tui"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	a := &app{
		stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr,
		getenv: os.Getenv, goos: runtime.GOOS,
		dial: dialEngine, ssh: systemSSH, builder: engine.CLIBuilder{},
		tool: runTool, lookPath: exec.LookPath, isTerminal: stdinIsTerminal, rawTerminal: rawTerminal, pick: tui.Pick,
	}
	os.Exit(a.run(ctx, os.Args[1:]))
}

// dockerEngine is what the commands need from the Docker Engine.
type dockerEngine interface {
	ops.Engine
	image.Exister
	VolumeExists(ctx context.Context, name string) (bool, error)
	SocketGID(ctx context.Context, image string) (int, error)
	TagImage(ctx context.Context, source, target string) error
	CDIDevices(ctx context.Context) (devices []string, known bool, err error)
	DeviceGIDs(ctx context.Context, image string, devices []string) ([]int, error)
	Logs(ctx context.Context, id string, opts engine.LogOptions, out io.Writer) error
	Exec(ctx context.Context, id string, spec engine.ExecSpec, streams engine.Streams) (int, error)
	Watch(ctx context.Context, label string) (changes <-chan struct{}, done <-chan error)
}

// app is one invocation of bh with its surroundings, so that tests can
// replace them.
type app struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	getenv         func(string) string
	goos           string

	// dial connects to the engine; the returned function disconnects.
	dial func(getenv func(string) string) (dockerEngine, func(), error)
	// ssh runs the ssh client on the terminal and returns its exit code.
	ssh     func(args []string) (int, error)
	builder image.Builder
	// tool runs a program of the host (code, ssh-keygen) and returns its output.
	tool func(name string, args []string) (string, error)
	// lookPath finds a program in PATH.
	lookPath func(name string) (string, error)
	// isTerminal: stdin is somebody typing.
	isTerminal func() bool
	// rawTerminal hands the terminal to a program running elsewhere: keys
	// go through unprocessed until restore is called. size reports the
	// terminal's size.
	rawTerminal func() (restore func(), size func() (height, width uint), err error)
	// pick asks the user to choose from a list.
	pick func(ctx context.Context, title string, choices []tui.Choice) (string, error)
}

// usageError is a wrong invocation: exit code 2, no "bh:" prefix games.
type usageError struct{ message string }

func (e usageError) Error() string { return e.message }

func usagef(format string, args ...any) error {
	return usageError{fmt.Sprintf(format, args...)}
}

// exitCode is a command that already said what it had to say (ssh passes
// the remote command's status through).
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

const usageText = `usage: bh [command] [arguments]

  (none)              the full-screen interface (everything below, in menus)
  create <name> [--from <env>] [--port N] [--publish] [--restart unless-stopped|no]
                      define a new environment (start it with up); --from
                      copies the settings and dev.Dockerfile of another one
  configure <env> [flags]
                      change settings (bh configure for the flags)
  rename <env> <new-name>
  up <env>            start the environment, creating what is missing
  down <env> | --all  stop it; containers and data stay
  restart <env> [--rebuild [--pull] [--no-cache]]
                      stop, recreate both containers, start; --rebuild
                      builds the image again first
  list                all environments and their state
  status <env>        one environment in detail
  ssh <env> [command] open a shell in the environment, or run a command
  open <env>          open it in VS Code
  key show | list | add | rename | remove | attach | detach
                      this machine's key, and other keys that may log in
                      (bh key for details)
  remote add | configure | remove
                      environments that run on another machine
                      (bh remote for details)
  logs <env> [--tail N] [--no-follow]
                      the output of both containers (follows; Ctrl-C ends)
  init <env>          run init.sh in the running environment now
  root-shell <env>    a root shell in the running environment (full
                      privileges, for troubleshooting)
  gpu list            the CDI devices the Docker engine offers
  path [<env>]        print the configuration directory (of an environment)
  reset <env> [--yes] delete home and nix, keep the workspace
  destroy <env> [--yes]
                      remove the environment and its data
  migrate [<name>]    take over an environment of the TypeScript version,
                      one at a time; without a name, list them
                      (--undo <name> hands one back; volumes are never removed)
  version             the build, and the Docker Engine bh talks to

  At a terminal, up, down, restart, logs, status, ssh and open ask which
  environment when none is named.
`

func (a *app) run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return a.finish(a.interactive(ctx))
	}
	commands := map[string]func(context.Context, []string) error{
		"create":     a.create,
		"configure":  a.configure,
		"rename":     a.rename,
		"up":         a.up,
		"down":       a.down,
		"restart":    a.restart,
		"list":       a.list,
		"status":     a.status,
		"ssh":        a.sshInto,
		"open":       a.open,
		"key":        a.key,
		"remote":     a.remote,
		"logs":       a.logs,
		"init":       a.initRun,
		"root-shell": a.rootShell,
		"gpu":        a.gpu,
		"path":       a.path,
		"destroy":    a.destroy,
		"migrate":    a.migrate,
		"version":    a.version,
	}
	name := args[0]
	switch name {
	case "help", "--help", "-h":
		fmt.Fprint(a.stdout, usageText)
		return 0
	case "--version", "-V":
		name = "version"
	}
	command, known := commands[name]
	if !known {
		fmt.Fprintf(a.stderr, "bh: unknown command %q\n\n%s", name, usageText)
		return 2
	}

	rest := args[1:]
	if localOnly, asks := pickable[name]; asks && len(rest) == 0 && a.isTerminal() {
		chosen, err := a.askWhich(ctx, name, localOnly)
		if err != nil {
			return a.finish(err)
		}
		rest = []string{chosen}
	}
	return a.finish(command(ctx, rest))
}

// finish turns the way a command ended into the exit status, saying what
// went wrong.
func (a *app) finish(err error) int {
	var usage usageError
	var code exitCode
	switch {
	case err == nil:
		return 0
	case errors.As(err, &code):
		return int(code)
	case errors.As(err, &usage):
		fmt.Fprintf(a.stderr, "bh: %v\n", err)
		return 2
	default:
		fmt.Fprintf(a.stderr, "bh: %v\n", err)
		return 1
	}
}

// store is the configuration directory this invocation works on.
func (a *app) store() (config.Store, error) {
	dir, err := config.Dir(a.goos, a.getenv)
	if err != nil {
		return config.Store{}, fmt.Errorf("cannot locate the configuration directory: %w", err)
	}
	return config.Store{Dir: dir}, nil
}

func resolveEndpoint(getenv func(string) string) (engine.Endpoint, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return engine.Endpoint{}, err
	}
	return engine.ResolveEndpoint(getenv, home)
}

func dialEngine(getenv func(string) string) (dockerEngine, func(), error) {
	endpoint, err := resolveEndpoint(getenv)
	if err != nil {
		return nil, nil, err
	}
	client, err := engine.Connect(endpoint)
	if err != nil {
		return nil, nil, err
	}
	return client, func() { client.Close() }, nil
}

// systemSSH runs the user's ssh client, so that everything in their
// ~/.ssh/config keeps applying.
func systemSSH(args []string) (int, error) {
	command := exec.Command("ssh", args...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := command.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exit):
		return exit.ExitCode(), nil
	case errors.Is(err, exec.ErrNotFound):
		return 0, errors.New("the ssh client was not found in PATH")
	default:
		return 0, err
	}
}

// version prints the bh build and which engine bh would talk to, and
// whether it answers.
func (a *app) version(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return usagef("version takes no arguments")
	}
	fmt.Fprintf(a.stdout, "bh %s (%s, %s/%s)\n", version, revision(), runtime.GOOS, runtime.GOARCH)

	endpoint, err := resolveEndpoint(a.getenv)
	if err != nil {
		return err
	}
	client, err := engine.Connect(endpoint)
	if err != nil {
		return err
	}
	defer client.Close()
	fmt.Fprintf(a.stdout, "engine: %s (from %s)\n", client.Host(), source(endpoint))

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := client.Info(ctx)
	if err != nil {
		return fmt.Errorf("engine not reachable: %w", err)
	}
	fmt.Fprintf(a.stdout, "  server %s, api %s, %s/%s, builder %s\n",
		info.Version, info.APIVersion, info.OS, info.Arch, info.Builder)
	return nil
}

func source(endpoint engine.Endpoint) string {
	switch endpoint.Source {
	case engine.SourceContext:
		return fmt.Sprintf("docker context %q", endpoint.Context)
	case engine.SourceHostEnv:
		return "DOCKER_HOST"
	default:
		return "the platform default"
	}
}

// revision is the commit the binary was built from, as recorded by the go
// command. "unknown" outside a git checkout (for example in a Nix build).
func revision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, dirty := "", false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			rev = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if dirty {
		rev += "+dirty"
	}
	return rev
}
