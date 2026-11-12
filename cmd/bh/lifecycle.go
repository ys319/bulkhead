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

// newLocal defines a local environment. With from, the settings and the
// dev.Dockerfile of that environment are copied; its data is not — the new
// one starts from fresh volumes, and its init.sh runs as for any new one.
// The keys come along because they are references into the registry.
func (a *app) newLocal(name, from string, edit func(*config.Environment)) (config.Store, config.Environment, error) {
	store, cfg, err := a.load()
	if err != nil {
		return store, config.Environment{}, err
	}
	if err := config.ValidateName(name); err != nil {
		return store, config.Environment{}, usagef("%q: %v", name, err)
	}
	id, err := newEnvID(store)
	if err != nil {
		return store, config.Environment{}, err
	}
	env := config.NewLocal(id, name, cfg.SuggestPort())
	var source *config.Environment
	if from != "" {
		found, err := cfg.Find(from)
		if err != nil {
			return store, env, err
		}
		if !found.Local() {
			return store, env, fmt.Errorf("%q is a remote environment (only local settings can be copied)", found.Name)
		}
		source = &found
		env = found
		env.ID, env.Name, env.SSHPort = id, name, cfg.SuggestPort()
	}
	if edit != nil {
		edit(&env)
	}
	if err := cfg.Check(env); err != nil {
		return store, env, err
	}
	if _, err := store.Init(); err != nil {
		return store, env, err
	}
	if err := store.SaveEnvironment(env); err != nil {
		return store, env, err
	}
	if source != nil {
		dockerfile, err := os.ReadFile(store.DevDockerfile(source.ID))
		if err == nil {
			err = os.WriteFile(store.DevDockerfile(id), dockerfile, 0o644)
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return store, env, fmt.Errorf("copying dev.Dockerfile from %q: %w", source.Name, err)
		}
	}
	a.syncSSH(store)
	return store, env, nil
}

func (a *app) create(_ context.Context, args []string) error {
	flags := flag.NewFlagSet("create", flag.ContinueOnError)
	port := flags.Int("port", 0, "")
	publish := flags.Bool("publish", false, "")
	restart := flags.String("restart", "", "")
	from := flags.String("from", "", "")
	rest, err := parseFlags(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef("usage: bh create <name> [--from <env>] [--port N] [--publish] [--restart unless-stopped|no]")
	}
	store, env, err := a.newLocal(rest[0], *from, func(env *config.Environment) {
		if *port != 0 {
			env.SSHPort = *port
		}
		if *publish {
			env.Publish = true
		}
		if *restart != "" {
			env.Restart = *restart
		}
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(a.stdout, "Created environment %q (id %s)\n", env.Name, env.ID)
	fmt.Fprintf(a.stdout, "  settings: %s\n", store.EnvFile(env.ID))
	fmt.Fprintf(a.stdout, "  ssh:      %s (start it with bh up %s)\n", sshAddress(env), env.Name)
	if env.DockerSocket {
		fmt.Fprintf(a.stderr, "bh: warning: %s: %s\n", env.Name, socketWarning)
	}
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
	machineKey, err := ensureMachineKey(store)
	if err != nil {
		return plan.Env{}, plan.Target{}, err
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
	a.syncSSH(store)
	fmt.Fprintf(a.stdout, "%s is up — ssh: %s (bh ssh %s)\n", env.Name, sshAddress(env), env.Name)
	return nil
}

func (a *app) down(ctx context.Context, args []string) error {
	if len(args) == 1 && args[0] == "--all" {
		return a.downAll(ctx)
	}
	ref, err := oneEnv("down", args)
	if err != nil {
		return usagef("usage: bh down <env> | --all")
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

// downAll stops every local environment that has a running container —
// broken ones included: their settings cannot be read, their containers
// can be stopped.
func (a *app) downAll(ctx context.Context) error {
	_, cfg, err := a.load()
	if err != nil {
		return err
	}
	type target struct{ id, name string }
	var targets []target
	for _, env := range cfg.Envs {
		if env.Local() {
			targets = append(targets, target{env.ID, env.Name})
		}
	}
	for _, broken := range cfg.Broken {
		if config.ValidID(broken.ID) {
			targets = append(targets, target{broken.ID, broken.ID})
		}
	}
	if len(targets) == 0 {
		fmt.Fprintln(a.stdout, "No local environments")
		return nil
	}
	eng, disconnect, err := a.dial(a.getenv)
	if err != nil {
		return err
	}
	defer disconnect()
	runner := ops.Runner{Engine: eng}

	stopped, failed := 0, 0
	for _, target := range targets {
		status, err := runner.Inspect(ctx, target.id)
		if err == nil && !active(status) {
			continue
		}
		if err == nil {
			err = runner.Down(ctx, target.id)
		}
		if err != nil {
			failed++
			fmt.Fprintf(a.stderr, "bh: %s: %v\n", target.name, err)
			continue
		}
		stopped++
		fmt.Fprintf(a.stdout, "%s is stopped\n", target.name)
	}
	if failed > 0 {
		return fmt.Errorf("%d environment(s) could not be stopped", failed)
	}
	if stopped == 0 {
		fmt.Fprintln(a.stdout, "Nothing was running")
	}
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
	id, name, local, port := "", ref, true, 0
	if env, err := cfg.Find(ref); err == nil {
		id, name, local, port = env.ID, env.Name, env.Local(), env.SSHPort
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
	if local && port != 0 {
		a.forgetHostKey(port)
	}
	a.syncSSH(store)
	fmt.Fprintf(a.stdout, "Destroyed %q\n", name)
	return nil
}
