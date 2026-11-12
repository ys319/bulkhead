package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/ys319/bulkhead/internal/config"
)

const remoteUsage = `usage: bh remote <subcommand>

  remote add <host:port/name> [--name <n>]
                            register an environment that runs on another
                            machine (name: its name there; --name picks a
                            different one here)
  remote configure <name> [--host <h>] [--port <n>]
  remote remove <name>      forget it (nothing on that machine is touched)

  A registered remote environment works with list, status, ssh and open.`

// addRemote registers a pointer to an environment on another machine.
func (a *app) addRemote(name, host string, port int) (config.Environment, error) {
	store, cfg, err := a.load()
	if err != nil {
		return config.Environment{}, err
	}
	if err := config.ValidateName(name); err != nil {
		return config.Environment{}, usagef("%q: %v", name, err)
	}
	id, err := newEnvID(store)
	if err != nil {
		return config.Environment{}, err
	}
	env := config.NewRemote(id, name, host, port)
	if err := cfg.Check(env); err != nil {
		return env, err
	}
	if _, err := store.Init(); err != nil {
		return env, err
	}
	if err := store.SaveEnvironment(env); err != nil {
		return env, err
	}
	a.syncSSH(store)
	return env, nil
}

// remoteEnv finds a registered remote environment.
func remoteEnv(cfg *config.Config, ref, localCommand string) (config.Environment, error) {
	env, err := cfg.Find(ref)
	if err != nil {
		return env, err
	}
	if env.Local() {
		return env, fmt.Errorf("%q is a local environment (use bh %s %s)", env.Name, localCommand, env.Name)
	}
	return env, nil
}

// configureRemote changes where a remote environment is reached.
func (a *app) configureRemote(ref string, host *string, port *int) (config.Environment, error) {
	store, cfg, err := a.load()
	if err != nil {
		return config.Environment{}, err
	}
	env, err := remoteEnv(cfg, ref, "configure")
	if err != nil {
		return env, err
	}
	if host != nil {
		env.Host = *host
	}
	if port != nil {
		env.SSHPort = *port
	}
	if err := cfg.Check(env); err != nil {
		return env, err
	}
	if err := store.SaveEnvironment(env); err != nil {
		return env, err
	}
	a.syncSSH(store)
	return env, nil
}

// removeRemote forgets a remote environment. Only the pointer goes, and
// bh remote add brings it back, so nothing asks for confirmation.
func (a *app) removeRemote(ref string) (config.Environment, error) {
	store, cfg, err := a.load()
	if err != nil {
		return config.Environment{}, err
	}
	env, err := remoteEnv(cfg, ref, "destroy")
	if err != nil {
		return env, err
	}
	if err := store.RemoveEnvironment(env.ID); err != nil {
		return env, err
	}
	a.syncSSH(store)
	return env, nil
}

func (a *app) remote(_ context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("%s", remoteUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		flags := flag.NewFlagSet("remote add", flag.ContinueOnError)
		name := flags.String("name", "", "")
		words, err := parseFlags(flags, rest)
		if err != nil {
			return err
		}
		if len(words) != 1 {
			return usagef("usage: bh remote add <host:port/name> [--name <n>]")
		}
		address, err := config.ParseRemoteAddress(words[0])
		if err != nil {
			return usagef("%v", err)
		}
		if *name == "" {
			*name = address.Name
		}
		env, err := a.addRemote(*name, address.Host, address.Port)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Registered remote environment %q → %s\n", env.Name, sshAddress(env))
		fmt.Fprintf(a.stdout, "Its machine has to let this machine's key in: bh key show here, bh key add + bh key attach there\n")
		return nil

	case "configure":
		flags := flag.NewFlagSet("remote configure", flag.ContinueOnError)
		host := flags.String("host", "", "")
		port := flags.Int("port", 0, "")
		words, err := parseFlags(flags, rest)
		if err != nil {
			return err
		}
		if len(words) != 1 {
			return usagef("usage: bh remote configure <name> [--host <h>] [--port <n>]")
		}
		var newHost *string
		var newPort *int
		flags.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "host":
				newHost = host
			case "port":
				newPort = port
			}
		})
		if newHost == nil && newPort == nil {
			return usagef("nothing to change (pass --host <h> and/or --port <n>)")
		}
		env, err := a.configureRemote(words[0], newHost, newPort)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Updated %q → %s\n", env.Name, sshAddress(env))
		return nil

	case "remove":
		if len(rest) != 1 {
			return usagef("usage: bh remote remove <name>")
		}
		env, err := a.removeRemote(rest[0])
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Removed %q (the environment on %s is untouched)\n", env.Name, env.Host)
		return nil
	}
	return usagef("%s", remoteUsage)
}
