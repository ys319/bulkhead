package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/ys319/bulkhead/containers"
	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/image"
	"github.com/ys319/bulkhead/internal/ops"
	"github.com/ys319/bulkhead/internal/plan"
	"github.com/ys319/bulkhead/internal/sshkey"
)

// parseFlags parses flags wherever they stand among the arguments
// (`bh create foo --port 1` and `bh create --port 1 foo` alike) and returns
// the other arguments.
func parseFlags(flags *flag.FlagSet, args []string) ([]string, error) {
	flags.SetOutput(io.Discard)
	var rest []string
	for {
		if err := flags.Parse(args); err != nil {
			return nil, usagef("%s: %v", flags.Name(), err)
		}
		args = flags.Args()
		if len(args) == 0 {
			return rest, nil
		}
		rest, args = append(rest, args[0]), args[1:]
	}
}

// oneEnv is the argument list of commands that take just an environment.
func oneEnv(command string, args []string) (string, error) {
	if len(args) != 1 {
		return "", usagef("usage: bh %s <env>", command)
	}
	return args[0], nil
}

func (a *app) create(_ context.Context, args []string) error {
	flags := flag.NewFlagSet("create", flag.ContinueOnError)
	port := flags.Int("port", 0, "")
	publish := flags.Bool("publish", false, "")
	restart := flags.String("restart", "unless-stopped", "")
	rest, err := parseFlags(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef("usage: bh create <name> [--port N] [--publish] [--restart unless-stopped|no]")
	}
	name := rest[0]
	if err := config.ValidateName(name); err != nil {
		return usagef("%q: %v", name, err)
	}

	store, err := a.store()
	if err != nil {
		return err
	}
	cfg, err := store.Load()
	if err != nil {
		return err
	}
	for _, env := range cfg.Envs {
		if env.Name == name {
			return fmt.Errorf("an environment named %q already exists", name)
		}
		if *port != 0 && env.Local() && env.SSHPort == *port {
			return fmt.Errorf("port %d is already the ssh port of %q", *port, env.Name)
		}
	}
	if *port == 0 {
		*port = cfg.SuggestPort()
	}

	id, err := newEnvID(store)
	if err != nil {
		return err
	}
	env := config.NewLocal(id, name, *port)
	env.Publish, env.Restart = *publish, *restart
	if _, err := store.Init(); err != nil {
		return err
	}
	if err := store.SaveEnvironment(env); err != nil {
		return err
	}

	fmt.Fprintf(a.stdout, "Created environment %q (id %s)\n", name, id)
	fmt.Fprintf(a.stdout, "  settings: %s\n", store.EnvFile(id))
	fmt.Fprintf(a.stdout, "  ssh:      %s (start it with bh up %s)\n", sshAddress(env), name)
	return nil
}

// newEnvID picks an id no environment directory uses yet.
func newEnvID(store config.Store) (string, error) {
	for {
		id, err := config.NewID()
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(store.EnvDir(id)); errors.Is(err, fs.ErrNotExist) {
			return id, nil
		} else if err != nil {
			return "", err
		}
	}
}

// localEnv loads the named environment and insists it is a local one.
func (a *app) localEnv(command, ref string) (config.Store, config.Environment, error) {
	store, err := a.store()
	if err != nil {
		return store, config.Environment{}, err
	}
	cfg, err := store.Load()
	if err != nil {
		return store, config.Environment{}, err
	}
	env, err := cfg.Find(ref)
	if err != nil {
		return store, env, err
	}
	if !env.Local() {
		return store, env, fmt.Errorf("%q is a remote environment: %s works on local ones", env.Name, command)
	}
	return store, env, nil
}

// containersFor turns the settings on disk into what the containers are
// created from, except the images.
func containersFor(store config.Store, env config.Environment) (plan.Env, plan.Target, error) {
	// Settings this build reads but cannot realize yet. Refusing is the
	// honest answer: starting without them would look like success.
	if env.GPU != "off" {
		return plan.Env{}, plan.Target{}, fmt.Errorf("%q asks for a GPU (gpu: %s), which this build does not support yet", env.Name, env.GPU)
	}
	if _, err := os.Stat(store.DevDockerfile(env.ID)); err == nil {
		return plan.Env{}, plan.Target{}, fmt.Errorf("%q has a dev.Dockerfile, which this build does not support yet", env.Name)
	}

	owner, err := store.Owner()
	if err != nil {
		return plan.Env{}, plan.Target{}, err
	}
	machineKey, err := sshkey.EnsureMachineKey(store.MachineKey(), "bulkhead")
	if err != nil {
		return plan.Env{}, plan.Target{}, fmt.Errorf("preparing the machine's ssh key: %w", err)
	}
	registry, err := store.LoadKeys()
	if err != nil {
		return plan.Env{}, plan.Target{}, err
	}
	registered, err := config.ResolveKeys(registry, env)
	if err != nil {
		return plan.Env{}, plan.Target{}, err
	}

	return plan.Env{
		ID: env.ID, Name: env.Name, SSHPort: env.SSHPort, Publish: env.Publish, Restart: env.Restart,
		AuthorizedKeys: append([]string{machineKey}, registered...),
		DockerSocket:   env.DockerSocket,
	}, plan.Target{Owner: owner}, nil
}

// socketWarning is said whenever an environment with the opt-in is started
// or shown: the opt-in is allowed, forgetting about it is not.
const socketWarning = "the Docker socket is mounted (dockerSocket: true) — this environment is not isolated from the host"

// realize fills in what only the engine can tell: the images (built unless
// it already has them) and, with the socket opt-in, the socket's group.
func (a *app) realize(ctx context.Context, eng dockerEngine, env *plan.Env, target *plan.Target) (err error) {
	target.HolderImage, err = image.Ensure(ctx, eng, a.builder,
		image.Source{Repo: "bulkhead-holder", FS: containers.FS, Dir: "holder"}, a.stderr)
	if err != nil {
		return err
	}
	target.DevImage, err = image.Ensure(ctx, eng, a.builder,
		image.Source{Repo: "bulkhead-dev-base", FS: containers.FS, Dir: "dev"}, a.stderr)
	if err != nil || !env.DockerSocket {
		return err
	}
	fmt.Fprintf(a.stderr, "bh: warning: %s: %s\n", env.Name, socketWarning)
	env.SocketGID, err = eng.SocketGID(ctx, target.HolderImage)
	return err
}

func (a *app) up(ctx context.Context, args []string) error {
	return a.start(ctx, "up", args, ops.Runner.Up)
}

func (a *app) restart(ctx context.Context, args []string) error {
	return a.start(ctx, "restart", args, ops.Runner.Restart)
}

func (a *app) start(ctx context.Context, command string, args []string,
	run func(ops.Runner, context.Context, plan.Env, plan.Target) error) error {
	ref, err := oneEnv(command, args)
	if err != nil {
		return err
	}
	store, env, err := a.localEnv(command, ref)
	if err != nil {
		return err
	}
	planned, target, err := containersFor(store, env)
	if err != nil {
		return err
	}
	eng, disconnect, err := a.dial(a.getenv)
	if err != nil {
		return err
	}
	defer disconnect()
	if err := a.realize(ctx, eng, &planned, &target); err != nil {
		return err
	}
	if err := run(ops.Runner{Engine: eng}, ctx, planned, target); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%s is up — ssh: %s (bh ssh %s)\n", env.Name, sshAddress(env), env.Name)
	return nil
}

func (a *app) down(ctx context.Context, args []string) error {
	ref, err := oneEnv("down", args)
	if err != nil {
		return err
	}
	_, env, err := a.localEnv("down", ref)
	if err != nil {
		return err
	}
	eng, disconnect, err := a.dial(a.getenv)
	if err != nil {
		return err
	}
	defer disconnect()
	if err := (ops.Runner{Engine: eng}).Down(ctx, env.ID); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%s is stopped (containers and data are kept)\n", env.Name)
	return nil
}

func (a *app) destroy(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("destroy", flag.ContinueOnError)
	yes := flags.Bool("yes", false, "")
	rest, err := parseFlags(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef("usage: bh destroy <env> [--yes]")
	}
	ref := rest[0]

	store, err := a.store()
	if err != nil {
		return err
	}
	cfg, err := store.Load()
	if err != nil {
		return err
	}
	// A broken environment can still be destroyed, by its id: its settings
	// are unreadable, its resources are not.
	id, name, local := "", ref, true
	if env, err := cfg.Find(ref); err == nil {
		id, name, local = env.ID, env.Name, env.Local()
	} else {
		for _, broken := range cfg.Broken {
			if broken.ID == ref {
				id = broken.ID
			}
		}
		if id == "" {
			return err
		}
	}

	if !*yes {
		what := "its containers, network and volumes (workspace, home, nix, ssh host keys) and its settings"
		if !local {
			what = "its settings (nothing on the remote machine is touched)"
		}
		fmt.Fprintf(a.stderr, "This removes %q: %s.\nType the name to confirm: ", name, what)
		answer, _ := bufio.NewReader(a.stdin).ReadString('\n')
		if strings.TrimSpace(answer) != name {
			return errors.New("not confirmed — nothing was removed")
		}
	}

	if local {
		eng, disconnect, err := a.dial(a.getenv)
		if err != nil {
			return err
		}
		defer disconnect()
		// Docker first: if that fails, the settings stay and point at what is left.
		if err := (ops.Runner{Engine: eng}).Destroy(ctx, id); err != nil {
			return err
		}
	}
	if err := store.RemoveEnvironment(id); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Destroyed %q\n", name)
	return nil
}
