package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ys319/bulkhead/internal/engine"
	"github.com/ys319/bulkhead/internal/ops"
	"github.com/ys319/bulkhead/internal/plan"
	"github.com/ys319/bulkhead/internal/snapshot"
)

// A reset takes part of an environment back to what the image provides,
// in two steps of how much goes:
//
//	the tools   the nix store, and in the home directory what belongs to
//	            it or comes back by itself (plan.HomeLeftOut: the nix
//	            profile, caches, the mark of init.sh). Nothing of the
//	            user's is lost; init.sh installs the tools again.
//	--home      the home directory as well. Logins, history and settings
//	            go — into a snapshot first, unless that is declined.
//
// The workspace is never part of a reset: that is bh destroy.

// resetDone is what a reset came to.
type resetDone struct {
	Home bool
	// Kept is the snapshot the home directory went into first; empty when
	// there was none to keep or keeping was declined.
	Kept string
}

// resetEnv resets the environment called ref. keep: with home, take a
// snapshot of the home directory before it goes.
func (a *app) resetEnv(ctx context.Context, ref string, home, keep bool) (resetDone, error) {
	store, env, err := a.localEnv("reset", ref)
	if err != nil {
		return resetDone{}, err
	}
	eng, disconnect, err := a.dial(a.getenv)
	if err != nil {
		return resetDone{}, err
	}
	defer disconnect()
	runner := ops.Runner{Engine: eng}
	// Stopped first: the home directory is read or changed below, and a
	// running environment has it open.
	if err := runner.Down(ctx, env.ID); err != nil {
		return resetDone{}, err
	}
	result := resetDone{Home: home}
	volume := plan.Volume(env.ID, "home")
	exists, err := eng.VolumeExists(ctx, volume)
	if err != nil {
		return result, err
	}

	if home {
		if keep && exists {
			taken, err := a.takeSnapshot(ctx, eng, store, env, snapshot.KindBeforeReset, "the home directory before a reset", "", []string{"home"})
			if err != nil {
				return result, fmt.Errorf("could not keep the home directory first, nothing was removed: %w", err)
			}
			result.Kept = strings.TrimSuffix(filepath.Base(taken.Path), snapshot.Extension)
		}
		return result, runner.Reset(ctx, env.ID, ops.ResetKeys)
	}

	// The tools only. What in the home directory points into the nix store
	// goes back to the image's — the links a fresh nix volume has targets
	// for — before the store itself goes. If this stops half way, running
	// it again finishes it.
	if exists {
		image, err := a.devImage(ctx, eng, store, env.ID, nil)
		if err != nil {
			return result, err
		}
		job := engine.VolumeTar{Image: image, Volume: volume, Target: plan.VolumeTarget("home")}
		if err := eng.RestoreFromImage(ctx, job, plan.HomeLeftOut); err != nil {
			return result, fmt.Errorf("taking the nix state in the home directory back to the image's (nothing else was changed): %w", err)
		}
	}
	return result, runner.Reset(ctx, env.ID, []string{"nix"})
}

func (a *app) reset(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("reset", flag.ContinueOnError)
	home := flags.Bool("home", false, "")
	noSnapshot := flags.Bool("no-snapshot", false, "")
	yes := flags.Bool("yes", false, "")
	rest, err := parseFlags(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 || (*noSnapshot && !*home) {
		return usagef("usage: bh reset <env> [--home [--no-snapshot]] [--yes]")
	}
	_, env, err := a.localEnv("reset", rest[0])
	if err != nil {
		return err
	}
	if !*yes {
		what := "the nix store and the caches"
		question := fmt.Sprintf("Remove the nix store and the caches of %q and install the tools again?\nThe environment is stopped. The workspace and what is in the home directory stay; at the next start init.sh runs again.", env.Name)
		if *home {
			what = "the home directory and the nix store"
			question = fmt.Sprintf("Delete the home directory and the nix store of %q and start over from the image?\nThe environment is stopped. The workspace is kept; logins, shell history and settings in the home directory are gone", env.Name)
			if *noSnapshot {
				question += ", and no snapshot of them is taken."
			} else {
				question += " — a snapshot of them is taken first."
			}
		}
		if !a.isTerminal() {
			return fmt.Errorf("reset deletes %s of %q (pass --yes to do it without asking)", what, env.Name)
		}
		if !a.confirm(question) {
			return errAborted
		}
	}
	done, err := a.resetEnv(ctx, env.Name, *home, !*noSnapshot)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, done.report(env.Name))
	fmt.Fprintf(a.stdout, "The next bh up %s runs init.sh again\n", env.Name)
	return nil
}

// report says what the reset did, in a sentence.
func (r resetDone) report(name string) string {
	if !r.Home {
		return fmt.Sprintf("Reset the tools of %q (the nix store and the caches are gone; the workspace and the home directory are kept)", name)
	}
	text := fmt.Sprintf("Reset %q (home and nix are deleted; the workspace is kept)", name)
	if r.Kept != "" {
		text += fmt.Sprintf(" — the home directory as it was is in the snapshot %s (bh rollback %s %s)", r.Kept, name, r.Kept)
	}
	return text
}
