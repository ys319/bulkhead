package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/ys319/bulkhead/containers"
	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/engine"
	"github.com/ys319/bulkhead/internal/image"
	"github.com/ys319/bulkhead/internal/inject"
	"github.com/ys319/bulkhead/internal/plan"
)

// Tokens reach an environment through two volumes that dev mounts
// read-only. The records on this machine (github.json, tokens.json) are
// what counts; the volumes are brought in line with them when a token
// changes and again before every start, which is what picks up a change
// made while Docker could not be reached.

// tokenHolder is the local environment a token is set on, shown for or
// taken from.
func tokenHolder(store config.Store, ref string) (config.Environment, error) {
	cfg, err := store.Load()
	if err != nil {
		return config.Environment{}, err
	}
	env, err := cfg.Find(ref)
	if err != nil {
		return env, err
	}
	if !env.Local() {
		return env, fmt.Errorf("%q is a remote environment — manage its tokens on its own machine", env.Name)
	}
	return env, nil
}

// tokenTarget is tokenHolder for giving the environment a token or taking
// one out of it: that writes into its volumes, which an archived
// environment does not have.
func tokenTarget(store config.Store, ref string) (config.Environment, error) {
	env, err := tokenHolder(store, ref)
	if err == nil && env.Archived != "" {
		err = archivedError(env)
	}
	return env, err
}

// volumeWriter changes the token volumes of one environment.
type volumeWriter struct {
	eng   dockerEngine
	owner string
	// image is what the scripts run in: the holder image, which has a shell
	// and is built on this machine anyway, so nothing is pulled for this.
	image string
	id    string
}

// volumeWriter connects to the engine and prepares the image. The returned
// function disconnects.
func (a *app) volumeWriter(ctx context.Context, store config.Store, id string) (volumeWriter, func(), error) {
	owner, err := store.Owner()
	if err != nil {
		return volumeWriter{}, nil, err
	}
	eng, disconnect, err := a.dial(a.getenv)
	if err != nil {
		return volumeWriter{}, nil, err
	}
	holder, err := image.Ensure(ctx, eng, a.builder,
		image.Source{Repo: holderRepo, FS: containers.FS, Dir: "holder"}, a.stderr)
	if err != nil {
		disconnect()
		return volumeWriter{}, nil, err
	}
	return volumeWriter{eng: eng, owner: owner, image: holder, id: id}, disconnect, nil
}

// job makes the engine's job of a script. The volume is created first if
// need be, with its labels: a token can be set before the first bh up, and
// the engine would otherwise create the volume itself, unlabelled.
func (w volumeWriter) job(ctx context.Context, job inject.Job) (engine.VolumeJob, error) {
	volume := plan.Volume(w.id, job.Key)
	if err := w.eng.EnsureVolume(ctx, volume, plan.Labels(w.owner, w.id, job.Key)); err != nil {
		return engine.VolumeJob{}, fmt.Errorf("preparing the %s volume: %w", job.Key, err)
	}
	return engine.VolumeJob{Image: w.image, Volume: volume, Target: job.Mount, Script: job.Script}, nil
}

// syncGitHub brings the github volume in line with github.json: the token
// and what the environment is told about it, or — after a revocation that
// could not empty the volume — nothing.
func (w volumeWriter) syncGitHub(ctx context.Context, store config.Store) error {
	state, err := store.LoadGitHub(w.id)
	if err != nil {
		return err
	}
	if state == nil {
		if !store.GitHubClearPending(w.id) {
			return nil
		}
		if err := w.clearGitHub(ctx); err != nil {
			return fmt.Errorf("clearing the revoked GitHub token from its volume: %w", err)
		}
		return store.SetGitHubClearPending(w.id, false)
	}
	job, err := w.job(ctx, inject.GitHubWrite)
	if err == nil {
		err = w.eng.WriteVolume(ctx, job, inject.GitHubInput(*state))
	}
	if err != nil {
		return fmt.Errorf("writing the GitHub token into its volume: %w", err)
	}
	return nil
}

// clearGitHub empties the github volume. The volume stays.
func (w volumeWriter) clearGitHub(ctx context.Context) error {
	job, err := w.job(ctx, inject.GitHubClear)
	if err != nil {
		return err
	}
	return w.eng.RunInVolume(ctx, job)
}

// syncTokens brings the tokens volume in line with tokens.json. Without
// the file nothing was ever put there.
func (w volumeWriter) syncTokens(ctx context.Context, store config.Store) error {
	tokens, exists, err := store.LoadTokens(w.id)
	if err != nil || !exists {
		return err
	}
	job, err := w.job(ctx, inject.TokensWrite)
	if err == nil {
		err = w.eng.WriteVolume(ctx, job, inject.TokensInput(tokens))
	}
	if err != nil {
		return fmt.Errorf("writing the tokens into their volume: %w", err)
	}
	return nil
}

// sync is both, before an environment starts. A failure stops the start:
// the environment would otherwise come up with a token that was taken away
// on this machine, or without one its user counts on.
func (w volumeWriter) sync(ctx context.Context, store config.Store) error {
	if err := w.syncGitHub(ctx, store); err != nil {
		return err
	}
	return w.syncTokens(ctx, store)
}

// applyTokens brings one of the environment's token volumes in line with
// its record, right after the record changed. Its error is not the
// command's: the record is saved, and the next bh up applies it.
func (a *app) applyTokens(ctx context.Context, store config.Store, id string, sync func(volumeWriter, context.Context, config.Store) error) error {
	writer, disconnect, err := a.volumeWriter(ctx, store, id)
	if err != nil {
		return err
	}
	defer disconnect()
	return sync(writer, ctx, store)
}

// readToken reads a token the user pastes: hidden when typed at a terminal,
// from standard input when piped.
func (a *app) readToken() (string, error) {
	if a.isTerminal() {
		return a.readSecret("Paste the token (input is hidden): ")
	}
	piped, err := io.ReadAll(io.LimitReader(a.stdin, 1<<20))
	return string(piped), err
}

// warn prints what did not stop a command but is left to do.
func (a *app) warn(warnings []string) {
	for _, warning := range warnings {
		fmt.Fprintf(a.stderr, "bh: warning: %s\n", warning)
	}
}

// oneLine folds an error that spans lines so that it fits inside a sentence.
func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}
