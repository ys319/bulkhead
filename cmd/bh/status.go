package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/engine"
	"github.com/ys319/bulkhead/internal/ops"
	"github.com/ys319/bulkhead/internal/sshconfig"
	"github.com/ys319/bulkhead/internal/tui"
)

// sshAddress is where the environment's ssh server is reached from here.
func sshAddress(env config.Environment) string {
	host := "127.0.0.1"
	if !env.Local() {
		host = env.Host
	}
	return host + ":" + strconv.Itoa(env.SSHPort)
}

func (a *app) list(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return usagef("list takes no arguments")
	}
	store, err := a.store()
	if err != nil {
		return err
	}
	cfg, err := store.Load()
	if err != nil {
		return err
	}
	if len(cfg.Envs) == 0 && len(cfg.Broken) == 0 {
		fmt.Fprintln(a.stdout, "No environments yet (make one with bh create <name>)")
		return nil
	}

	// One engine for all rows. If it cannot be reached the list still shows
	// what is configured, with the state unknown.
	var runner *ops.Runner
	for _, env := range cfg.Envs {
		if !env.Local() {
			continue
		}
		eng, disconnect, err := a.dial(a.getenv)
		if err != nil {
			fmt.Fprintf(a.stderr, "bh: the state is unknown: %v\n", err)
			break
		}
		defer disconnect()
		runner = &ops.Runner{Engine: eng}
		break
	}

	table := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "NAME\tSTATE\tSSH\tNOTE")
	for _, env := range cfg.Envs {
		state := "remote"
		if env.Archived != "" {
			state = "archived"
		} else if env.Local() {
			state = "unknown"
			if runner != nil {
				status, err := runner.Inspect(ctx, env.ID)
				if err != nil {
					fmt.Fprintf(a.stderr, "bh: the state is unknown: %v\n", err)
					runner = nil
				} else {
					state = string(status.State())
				}
			}
		}
		var notes []string
		if env.DockerSocket {
			notes = append(notes, "docker socket (not isolated)")
		}
		if forwarding := forwardNote(store, env); forwarding != "" {
			notes = append(notes, forwarding)
		}
		note := strings.Join(notes, "; ")
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", env.Name, state, sshAddress(env), note)
	}
	for _, broken := range cfg.Broken {
		fmt.Fprintf(table, "%s\tbroken\t-\t\n", broken.ID)
	}
	if err := table.Flush(); err != nil {
		return err
	}
	for _, broken := range cfg.Broken {
		fmt.Fprintf(a.stderr, "bh: %s is broken — %s\n", broken.ID, broken.Problem)
	}
	return nil
}

func (a *app) status(ctx context.Context, args []string) error {
	ref, err := oneEnv("status", args)
	if err != nil {
		return err
	}
	store, err := a.store()
	if err != nil {
		return err
	}
	cfg, err := store.Load()
	if err != nil {
		return err
	}
	env, err := cfg.Find(ref)
	if err != nil {
		return err
	}

	row := func(key, value string) { fmt.Fprintf(a.stdout, "%-9s %s\n", key+":", value) }
	// The saved forwards, last: what this machine and the environment are
	// given of each other.
	forwards := func() {
		for _, state := range forwardStates(store, env) {
			row("forward", fmt.Sprintf("%s (%s): %s", state.Name, state.label(), state.Summary()))
		}
	}
	row("name", env.Name)
	row("id", env.ID)
	if !env.Local() {
		row("type", "remote")
		row("ssh", sshAddress(env))
		forwards()
		return nil
	}

	if env.Archived != "" {
		row("state", "archived")
		path := filepath.Join(store.SnapshotsDir(env.ID), env.Archived)
		if info, err := os.Stat(path); err != nil {
			row("snapshot", fmt.Sprintf("%s — MISSING (%v)", path, err))
		} else {
			row("snapshot", fmt.Sprintf("%s (%s)", path, tui.HumanBytes(info.Size())))
		}
		fmt.Fprintf(a.stdout, "\nbh unarchive %s brings it back.\n", env.Name)
		return nil
	}

	eng, disconnect, err := a.dial(a.getenv)
	if err != nil {
		return err
	}
	defer disconnect()
	current, err := (ops.Runner{Engine: eng}).Inspect(ctx, env.ID)
	if err != nil {
		return err
	}
	row("state", string(current.State()))
	address := sshAddress(env)
	if env.Publish {
		address = "0.0.0.0:" + strconv.Itoa(env.SSHPort) + " (published to the network)"
	}
	row("ssh", address)
	if env.DockerSocket {
		row("boundary", "none — "+socketWarning)
	}
	// Sizes are worth the engine's walk over the volumes here, where one
	// environment is asked about; the list does without.
	if sizes, err := eng.VolumeSizes(ctx); err == nil {
		if summary, has := tui.DiskSummary(env.ID, sizes); has {
			row("disk", summary)
		}
	}
	row("holder", describe(current.Holder, time.Now()))
	row("dev", describe(current.Dev, time.Now()))
	forwards()
	if current.State() == ops.StateDegraded {
		fmt.Fprintf(a.stdout, "\nThe two containers do not add up to a working environment; bh up %s repairs it.\n", env.Name)
	}
	return nil
}

// describe is one container in a few words: "running (healthy), up 3m".
func describe(found *engine.Container, now time.Time) string {
	if found == nil {
		return "not created"
	}
	text := found.Status
	if found.Running() {
		if found.Health != "" {
			text += " (" + found.Health + ")"
		}
		if !found.StartedAt.IsZero() {
			text += ", up " + roughly(now.Sub(found.StartedAt))
		}
	}
	return text
}

// roughly rounds a duration to its largest unit: 45s, 3m, 5h, 2d.
func roughly(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	}
	return strconv.Itoa(int(d.Hours())/24) + "d"
}

func (a *app) path(_ context.Context, args []string) error {
	store, err := a.store()
	if err != nil {
		return err
	}
	switch len(args) {
	case 0:
		fmt.Fprintln(a.stdout, store.Dir)
		return nil
	case 1:
		cfg, err := store.Load()
		if err != nil {
			return err
		}
		// A broken environment's directory is exactly what one needs to find.
		for _, broken := range cfg.Broken {
			if broken.ID == args[0] {
				fmt.Fprintln(a.stdout, store.EnvDir(broken.ID))
				return nil
			}
		}
		env, err := cfg.Find(args[0])
		if err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, store.EnvDir(env.ID))
		return nil
	}
	return usagef("usage: bh path [<env>]")
}

// sshArgs are the arguments of the ssh client for the environment: the same
// terms as its entry in the ssh configuration. IdentitiesOnly keeps an
// agent full of keys from using up the server's tries before the machine
// key gets its turn; HostKeyAlias files the host key under the environment
// instead of under its port.
func sshArgs(store config.Store, env config.Environment, options ...string) []string {
	host := env.Host
	if env.Local() {
		host = "127.0.0.1"
	}
	args := []string{
		"-i", store.MachineKey(),
		"-p", strconv.Itoa(env.SSHPort),
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "IdentitiesOnly=yes",
		"-o", "HostKeyAlias=" + sshconfig.HostKeyAlias(env.ID),
	}
	for _, option := range options {
		args = append(args, "-o", option)
	}
	return append(args, "developer@"+host)
}

func (a *app) sshInto(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("usage: bh ssh <env> [command...]")
	}
	store, cfg, err := a.load()
	if err != nil {
		return err
	}
	env, err := cfg.Find(args[0])
	if err != nil {
		return err
	}
	if err := a.requireRunning(ctx, env); err != nil {
		return err
	}
	code, err := a.ssh(append(sshArgs(store, env), args[1:]...))
	if err != nil {
		return err
	}
	if code != 0 {
		return exitCode(code)
	}
	return nil
}
