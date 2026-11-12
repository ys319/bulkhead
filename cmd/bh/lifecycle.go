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
	"slices"
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

// initScriptTemplate is what a new init.sh starts as. The file is the
// user's from then on.
const initScriptTemplate = `#!/usr/bin/env zsh
# bulkhead init script — runs once as the developer user on the first boot of
# a fresh home volume (bh create / bh reset), before sshd starts. A failure
# aborts the boot; the next start retries. This file is yours to edit.
# It is run with zsh, so nix from the image is on PATH.
#
# Example: deploy your Claude Code config / dotfiles
#   git clone https://github.com/<you>/dotfiles.git ~/dotfiles
#   cd ~/dotfiles && ./deploy.sh
`

// ensureInitScript returns the path of init.sh, shared by all environments,
// making it from the template when it is missing. It has to exist before a
// container mounts it. An existing file is never touched, permissions
// included.
func ensureInitScript(store config.Store) (string, error) {
	path := store.InitScript()
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(store.Dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(initScriptTemplate), 0o755); err != nil {
		return "", err
	}
	// WriteFile's mode is cut by the umask; with 077 the developer user in
	// the container (another uid on some Linux hosts) could not read it,
	// and the first boot would stop.
	return path, os.Chmod(path, 0o755)
}

// containersFor turns the settings on disk into what the containers are
// created from, except what only the engine can tell (realize).
func containersFor(store config.Store, env config.Environment) (plan.Env, plan.Target, error) {
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
	if env.GPU == "cdi" && len(env.CDIDevices) == 0 {
		return plan.Env{}, plan.Target{}, fmt.Errorf("%q uses gpu: cdi but no device is chosen — bh configure %s --cdi-devices <name,...> (bh gpu list shows them)", env.Name, env.Name)
	}
	initScript, err := ensureInitScript(store)
	if err != nil {
		return plan.Env{}, plan.Target{}, fmt.Errorf("preparing init.sh: %w", err)
	}

	return planEnv(env, append([]string{machineKey}, registered...), initScript), plan.Target{Owner: owner}, nil
}

// planEnv is the settings as the plan takes them. It is one function so
// that bh doctor compares the containers with exactly what bh up creates
// them from.
func planEnv(env config.Environment, keys []string, initScript string) plan.Env {
	return plan.Env{
		ID: env.ID, Name: env.Name, SSHPort: env.SSHPort, Publish: env.Publish, Restart: env.Restart,
		AuthorizedKeys: keys,
		DockerSocket:   env.DockerSocket,
		InitScript:     initScript,
		GPU:            env.GPU,
		CDIDevices:     env.CDIDevices,
	}
}

// socketWarning is said whenever an environment with the opt-in is started
// or shown: the opt-in is allowed, forgetting about it is not.
const socketWarning = "the Docker socket is mounted (dockerSocket: true) — this environment is not isolated from the host"

// Image names. The base of dev also gets the plain name devBaseAlias, which
// is what a dev.Dockerfile starts FROM.
const (
	holderRepo   = "bulkhead-holder"
	devBaseRepo  = "bulkhead-dev-base"
	devBaseAlias = devBaseRepo + ":latest"
)

// devImageRepo is the image of an environment with its own dev.Dockerfile.
func devImageRepo(id string) string { return "bulkhead-dev-" + id }

// The sources of the images: the two embedded in bh, and an environment's
// dev.Dockerfile on top of the base it was given. Their references are
// what tells a container of this bh from one of another (bh doctor).
func holderSource() image.Source {
	return image.Source{Repo: holderRepo, FS: containers.FS, Dir: "holder"}
}

func devBaseSource() image.Source {
	return image.Source{Repo: devBaseRepo, FS: containers.FS, Dir: "dev"}
}

func devSource(store config.Store, id, base string) image.Source {
	return image.Source{
		Repo: devImageRepo(id), FS: os.DirFS(store.EnvDir(id)), Dir: ".",
		Dockerfile: "dev.Dockerfile", Skip: keptOutOfBuilds, Base: base,
	}
}

// keptOutOfBuilds: files in an environment's directory that are bh's, not
// part of the user's build context. The token files among them hold secrets.
func keptOutOfBuilds(name string) bool {
	switch name {
	case "env.json", "github.json", "tokens.json":
		return true
	}
	return strings.HasSuffix(name, ".tmp") || strings.HasPrefix(name, ".")
}

// realize fills in what only the engine can tell: the images (built unless
// it already has them, or in any case with rebuild), the socket's group
// with the opt-in, and the groups of the GPU devices.
func (a *app) realize(ctx context.Context, eng dockerEngine, store config.Store, env *plan.Env, target *plan.Target, rebuild *image.Options) (err error) {
	target.HolderImage, err = image.Ensure(ctx, eng, a.builder, holderSource(), a.stderr)
	if err != nil {
		return err
	}
	target.DevImage, err = image.EnsureWith(ctx, eng, a.builder, devBaseSource(), rebuild, a.stderr)
	if err != nil {
		return err
	}

	// The environment's own image, when it has a dev.Dockerfile: the user's
	// steps on top of the base.
	if _, statErr := os.Stat(store.DevDockerfile(env.ID)); statErr == nil {
		if err := eng.TagImage(ctx, target.DevImage, devBaseAlias); err != nil {
			return fmt.Errorf("naming the base image for dev.Dockerfile: %w", err)
		}
		var again *image.Options
		if rebuild != nil {
			// Not Pull: the base exists on this machine only.
			again = &image.Options{NoCache: rebuild.NoCache}
		}
		target.DevImage, err = image.EnsureWith(ctx, eng, a.builder, devSource(store, env.ID, target.DevImage), again, a.stderr)
		if err != nil {
			return fmt.Errorf("building %s: %w", store.DevDockerfile(env.ID), err)
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return statErr
	}

	if env.DockerSocket {
		fmt.Fprintf(a.stderr, "bh: warning: %s: %s\n", env.Name, socketWarning)
		if env.SocketGID, err = eng.SocketGID(ctx, target.HolderImage); err != nil {
			return err
		}
	}
	if env.GPU == "cdi" {
		known, reported, err := eng.CDIDevices(ctx)
		if err != nil {
			return err
		}
		if reported {
			var missing []string
			for _, device := range env.CDIDevices {
				if !slices.Contains(known, device) {
					missing = append(missing, device)
				}
			}
			if len(missing) > 0 {
				available := strings.Join(known, ", ")
				if available == "" {
					available = "(none)"
				}
				return fmt.Errorf("CDI device not found on this Docker engine: %s\navailable: %s", strings.Join(missing, ", "), available)
			}
		}
		if env.DeviceGIDs, err = eng.DeviceGIDs(ctx, target.HolderImage, env.CDIDevices); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) up(ctx context.Context, args []string) error {
	ref, err := oneEnv("up", args)
	if err != nil {
		return err
	}
	return a.start(ctx, "up", ref, nil, ops.Runner.Up)
}

func (a *app) restart(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("restart", flag.ContinueOnError)
	rebuild := flags.Bool("rebuild", false, "")
	pull := flags.Bool("pull", false, "")
	noCache := flags.Bool("no-cache", false, "")
	rest, err := parseFlags(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef("usage: bh restart <env> [--rebuild [--pull] [--no-cache]]")
	}
	var options *image.Options
	switch {
	case *rebuild:
		options = &image.Options{Pull: *pull, NoCache: *noCache}
	case *pull || *noCache:
		return usagef("--pull and --no-cache go with --rebuild")
	}
	return a.start(ctx, "restart", rest[0], options, ops.Runner.Restart)
}

func (a *app) start(ctx context.Context, command, ref string, rebuild *image.Options,
	run func(ops.Runner, context.Context, plan.Env, plan.Target) error) error {
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
	if err := a.realize(ctx, eng, store, &planned, &target, rebuild); err != nil {
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
