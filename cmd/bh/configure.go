package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/ys319/bulkhead/internal/config"
)

// onOff is a flag that takes "on" or "off", so that turning something off
// is as explicit as turning it on.
type onOff struct{ value *bool }

func (f *onOff) String() string { return "" }

func (f *onOff) Set(text string) error {
	switch text {
	case "on":
		f.value = ptr(true)
	case "off":
		f.value = ptr(false)
	default:
		return fmt.Errorf("takes on or off")
	}
	return nil
}

func ptr[T any](value T) *T { return &value }

// applyChanges edits a local environment's settings. The settings its
// containers were created from cannot change while it runs.
func (a *app) applyChanges(ctx context.Context, ref string, changes config.Changes) (config.Environment, error) {
	store, cfg, err := a.load()
	if err != nil {
		return config.Environment{}, err
	}
	env, err := cfg.Find(ref)
	if err != nil {
		return env, err
	}
	if !env.Local() {
		return env, fmt.Errorf("%q is a remote environment (change it with bh remote configure %s)", env.Name, env.Name)
	}
	updated := env.With(changes)
	if err := cfg.Check(updated); err != nil {
		return env, err
	}
	if locked := changes.Locked(); len(locked) > 0 {
		status, err := a.inspect(ctx, env)
		if err != nil {
			return env, fmt.Errorf("cannot tell whether %q is running: %w", env.Name, err)
		}
		if active(status) {
			return env, fmt.Errorf("%q is running — stop it first to change %s (bh down %s, then bh up %s)",
				env.Name, strings.Join(locked, ", "), env.Name, env.Name)
		}
	}
	if err := store.SaveEnvironment(updated); err != nil {
		return env, err
	}
	if changes.SSHPort != nil {
		a.syncSSH(store)
	}
	return updated, nil
}

const configureUsage = `usage: bh configure <env> [flags]

  --publish on|off        listen on all interfaces instead of loopback only
  --port <n>              the ssh port on this machine
  --restart unless-stopped|no
  --gpu off|legacy|cdi    give the environment the GPUs
  --cdi-devices <name,...>  the devices for --gpu cdi (see bh gpu list)
  --docker-socket on|off  on lets the environment control the host's Docker,
                          so it is no longer isolated`

func (a *app) configure(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("configure", flag.ContinueOnError)
	var publish, socket onOff
	flags.Var(&publish, "publish", "")
	flags.Var(&socket, "docker-socket", "")
	port := flags.Int("port", 0, "")
	restart := flags.String("restart", "", "")
	gpu := flags.String("gpu", "", "")
	devices := flags.String("cdi-devices", "", "")
	rest, err := parseFlags(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef("%s", configureUsage)
	}

	changes := config.Changes{Publish: publish.value, DockerSocket: socket.value}
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "port":
			changes.SSHPort = port
		case "restart":
			changes.Restart = restart
		case "gpu":
			changes.GPU = gpu
		case "cdi-devices":
			list := []string{}
			for _, device := range strings.Split(*devices, ",") {
				if device = strings.TrimSpace(device); device != "" {
					list = append(list, device)
				}
			}
			changes.CDIDevices = &list
		}
	})
	if changes.Empty() {
		return usagef("nothing to change\n\n%s", configureUsage)
	}

	env, err := a.applyChanges(ctx, rest[0], changes)
	if err != nil {
		return err
	}
	// Everything configure changes is locked while running, so having saved
	// means it is stopped.
	fmt.Fprintf(a.stdout, "Updated %q (applies at the next bh up %s)\n", env.Name, env.Name)
	if changes.DockerSocket != nil && *changes.DockerSocket {
		fmt.Fprintf(a.stderr, "bh: warning: %s: %s\n", env.Name, socketWarning)
	}
	return nil
}

// renameEnv gives an environment another name. Its id — and with it every
// Docker resource and its directory — stays, so nothing has to be moved.
func (a *app) renameEnv(ref, name string) (old, renamed config.Environment, err error) {
	store, cfg, err := a.load()
	if err != nil {
		return old, renamed, err
	}
	if old, err = cfg.Find(ref); err != nil {
		return old, renamed, err
	}
	if err := config.ValidateName(name); err != nil {
		return old, renamed, usagef("%q: %v", name, err)
	}
	renamed = old
	renamed.Name = name
	if err := cfg.Check(renamed); err != nil {
		return old, renamed, err
	}
	if err := store.SaveEnvironment(renamed); err != nil {
		return old, renamed, err
	}
	a.syncSSH(store)
	return old, renamed, nil
}

func (a *app) rename(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return usagef("usage: bh rename <env> <new-name>")
	}
	old, renamed, err := a.renameEnv(args[0], args[1])
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Renamed %q to %q (ssh: bulkhead-%s)\n", old.Name, renamed.Name, renamed.Name)
	if renamed.Local() {
		// The hostname is part of what the containers are created from.
		if status, err := a.inspect(ctx, renamed); err == nil && (status.Holder != nil || status.Dev != nil) {
			fmt.Fprintf(a.stdout, "The hostname inside follows when the containers are recreated (bh restart %s)\n", renamed.Name)
		}
	}
	return nil
}
