package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/plan"
	"github.com/ys319/bulkhead/internal/sshconfig"
)

// hasVSCode: the `code` command is in PATH.
func (a *app) hasVSCode() bool {
	_, err := a.lookPath("code")
	return err == nil
}

// openInEditor opens the environment in VS Code, which reaches it through
// the ssh client's configuration: key, port and user come from the Host
// entry bh keeps there.
func (a *app) openInEditor(ctx context.Context, store config.Store, env config.Environment) error {
	if !a.hasVSCode() {
		return errors.New(`VS Code's code command was not found in PATH (in VS Code, run "Shell Command: Install 'code' command in PATH")`)
	}
	if err := a.requireRunning(ctx, env); err != nil {
		return err
	}
	if _, err := ensureMachineKey(store); err != nil {
		return err
	}
	// The entry is what VS Code connects through, so here a failure counts.
	if err := a.writeSSH(store); err != nil {
		return fmt.Errorf("the ssh configuration could not be updated: %w", err)
	}
	// The folder is what makes this a window of its own: with the remote
	// alone, a VS Code that is already open is brought to the front and
	// nothing else happens.
	_, err := a.tool("code", []string{"--remote", "ssh-remote+" + sshconfig.Alias(env.Name), plan.Workspace})
	return err
}

func (a *app) open(ctx context.Context, args []string) error {
	ref, err := oneEnv("open", args)
	if err != nil {
		return err
	}
	store, cfg, err := a.load()
	if err != nil {
		return err
	}
	env, err := cfg.Find(ref)
	if err != nil {
		return err
	}
	if err := a.openInEditor(ctx, store, env); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Opened %q in VS Code\n", env.Name)
	return nil
}
