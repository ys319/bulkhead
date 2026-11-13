package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/ys319/bulkhead/containers"
	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/engine"
	"github.com/ys319/bulkhead/internal/huggingface"
	"github.com/ys319/bulkhead/internal/image"
	"github.com/ys319/bulkhead/internal/inject"
	"github.com/ys319/bulkhead/internal/plan"
	"github.com/ys319/bulkhead/internal/vault"
)

// What an environment is handed — variables, secrets, tokens — is kept on
// this machine and written into two volumes that dev mounts read-only. The
// records here are what counts; the volumes are brought in line with them
// when something changes and again before every start, which is what picks
// up a change made while Docker could not be reached.
//
// The values of secrets and tokens are not in the records but in the vault:
// the operating system's credential store, or a file where this machine has
// none (bh secret store).

// secretStoreKind is where this machine keeps values. decide: a value is
// about to be written, so if nothing is decided yet, decide now — the
// credential store if it can be used, and an error that says how to choose
// the file otherwise. Never the file by itself: where secrets end up is not
// something to fall back on quietly.
func (a *app) secretStoreKind(store config.Store, decide bool) (string, error) {
	if forced := a.getenv("BH_SECRET_STORE"); forced != "" {
		if forced != vault.KindOS && forced != vault.KindFile {
			return "", fmt.Errorf("BH_SECRET_STORE is %q; it is %s or %s", forced, vault.KindOS, vault.KindFile)
		}
		return forced, nil
	}
	kind, err := store.SecretStore()
	if err != nil || kind != "" || !decide {
		return kind, err
	}
	owner, err := store.Init()
	if err != nil {
		return "", err
	}
	if err := a.probeVault(owner + "/"); err != nil {
		return "", fmt.Errorf("the credential store of this machine cannot be used (%s)\nTo keep the values of tokens and secrets in a file instead (readable only by you): bh secret store file", oneLine(err))
	}
	return vault.KindOS, store.SetSecretStore(vault.KindOS)
}

// vaultOf is where the environment's values are, for the kind of store.
func (a *app) vaultOf(store config.Store, id, kind string) (vault.Store, error) {
	switch kind {
	case vault.KindFile:
		return vault.File{Path: store.ValuesFile(id)}, nil
	case vault.KindOS:
		owner, err := store.Owner()
		if err != nil {
			return nil, err
		}
		// By configuration directory and environment: another directory's
		// values (a test's) are not this one's.
		return a.osVault(owner + "/" + id + "/"), nil
	}
	// Nothing was ever stored: reading finds nothing.
	return vault.Memory{}, nil
}

// credentials are an environment's tokens and secrets, with where their
// values are.
type credentials struct {
	store   config.Store
	env     config.Environment
	vault   vault.Store
	Tokens  []config.Token
	Secrets []config.Secret
}

// credentialsOf reads the environment's tokens and secrets. Records of
// earlier builds are taken over first. decide: see secretStoreKind.
func (a *app) credentialsOf(store config.Store, env config.Environment, decide bool) (*credentials, error) {
	legacy, err := store.HasLegacyCredentials(env.ID)
	if err != nil {
		return nil, err
	}
	kind, err := a.secretStoreKind(store, decide || legacy)
	if err != nil {
		return nil, err
	}
	creds := &credentials{store: store, env: env}
	if creds.vault, err = a.vaultOf(store, env.ID, kind); err != nil {
		return nil, err
	}
	if legacy {
		if err := store.ConvertLegacyCredentials(env.ID, creds.vault.Set); err != nil {
			return nil, fmt.Errorf("taking over the token records of %q: %w", env.Name, err)
		}
	}
	if creds.Tokens, err = store.LoadTokens(env.ID); err != nil {
		return nil, err
	}
	if creds.Secrets, err = store.LoadSecrets(env.ID); err != nil {
		return nil, err
	}
	return creds, nil
}

// token is the environment's token of the service; nil when it has none.
func (c *credentials) token(service string) *config.Token {
	at := slices.IndexFunc(c.Tokens, func(t config.Token) bool { return t.Service == service })
	if at < 0 {
		return nil
	}
	return &c.Tokens[at]
}

// putToken records the token and keeps its value.
func (c *credentials) putToken(token config.Token, value string) error {
	if err := c.vault.Set(config.TokenKey(token.Service), value); err != nil {
		return fmt.Errorf("keeping the token: %w", err)
	}
	c.Tokens = append(slices.DeleteFunc(slices.Clone(c.Tokens), func(t config.Token) bool { return t.Service == token.Service }), token)
	return c.store.SaveTokens(c.env.ID, c.Tokens)
}

// dropToken forgets the token and its value.
func (c *credentials) dropToken(service string) error {
	c.Tokens = slices.DeleteFunc(slices.Clone(c.Tokens), func(t config.Token) bool { return t.Service == service })
	if err := c.store.SaveTokens(c.env.ID, c.Tokens); err != nil {
		return err
	}
	return c.vault.Delete(config.TokenKey(service))
}

// forget removes every value of the environment from the vault: it is
// being destroyed, and the credential store would keep them for good.
func (c *credentials) forget() error {
	var problems []string
	for _, token := range c.Tokens {
		if err := c.vault.Delete(config.TokenKey(token.Service)); err != nil {
			problems = append(problems, oneLine(err))
		}
	}
	for _, secret := range c.Secrets {
		if err := c.vault.Delete(config.SecretKey(secret.Name)); err != nil {
			problems = append(problems, oneLine(err))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// variables are what the environment gets as environment variables: its
// variables, its secrets, and the tokens that are nothing more inside than
// a variable. GitHub's is not among them — it has a volume of its own,
// which the git credential helper reads too.
func (c *credentials) variables() ([]inject.Variable, error) {
	var out []inject.Variable
	names := make([]string, 0, len(c.env.Vars))
	for name := range c.env.Vars {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, inject.Variable{Name: name, Value: c.env.Vars[name]})
	}
	for _, secret := range c.Secrets {
		value, err := c.vault.Get(config.SecretKey(secret.Name))
		if err != nil {
			return nil, fmt.Errorf("the value of the secret %s: %w", secret.Name, err)
		}
		out = append(out, inject.Variable{Name: secret.Name, Value: value})
	}
	if c.token(config.ServiceHuggingFace) != nil {
		value, err := c.vault.Get(config.TokenKey(config.ServiceHuggingFace))
		if err != nil {
			return nil, fmt.Errorf("the Hugging Face token: %w", err)
		}
		out = append(out, inject.Variable{Name: huggingface.Variable, Value: value})
	}
	return out, nil
}

// handedTo is the local environment that is given a variable, a secret or
// a token, or has one taken away. That writes into its volumes, which an
// archived environment does not have.
func handedTo(store config.Store, ref string) (config.Environment, error) {
	env, err := holderOf(store, ref)
	if err == nil && env.Archived != "" {
		err = archivedError(env)
	}
	return env, err
}

// holderOf is the local environment whose variables, secrets or tokens are
// asked about.
func holderOf(store config.Store, ref string) (config.Environment, error) {
	cfg, err := store.Load()
	if err != nil {
		return config.Environment{}, err
	}
	env, err := cfg.Find(ref)
	if err != nil {
		return env, err
	}
	if !env.Local() {
		return env, fmt.Errorf("%q is a remote environment — manage what it is handed on its own machine", env.Name)
	}
	return env, nil
}

// volumeWriter changes the two volumes of one environment.
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
// need be, with its labels: something can be set before the first bh up,
// and the engine would otherwise create the volume itself, unlabelled.
func (w volumeWriter) job(ctx context.Context, job inject.Job) (engine.VolumeJob, error) {
	volume := plan.Volume(w.id, job.Key)
	if err := w.eng.EnsureVolume(ctx, volume, plan.Labels(w.owner, w.id, job.Key)); err != nil {
		return engine.VolumeJob{}, fmt.Errorf("preparing the %s volume: %w", job.Key, err)
	}
	return engine.VolumeJob{Image: w.image, Volume: volume, Target: job.Mount, Script: job.Script}, nil
}

// syncGitHub brings the github volume in line with the records: the token
// and what the environment is told about it, or — when a token was taken
// away while the volume could not be emptied — nothing.
func (w volumeWriter) syncGitHub(ctx context.Context, creds *credentials) error {
	token := creds.token(config.ServiceGitHub)
	if token == nil {
		if !creds.store.Pending(w.id, config.PendingGitHub) {
			return nil
		}
		job, err := w.job(ctx, inject.GitHubClear)
		if err == nil {
			err = w.eng.RunInVolume(ctx, job)
		}
		if err != nil {
			return fmt.Errorf("clearing the GitHub token from its volume: %w", err)
		}
		return creds.store.SetPending(w.id, config.PendingGitHub, false)
	}
	value, err := creds.vault.Get(config.TokenKey(config.ServiceGitHub))
	if err != nil {
		return fmt.Errorf("the GitHub token: %w", err)
	}
	job, err := w.job(ctx, inject.GitHubWrite)
	if err == nil {
		err = w.eng.WriteVolume(ctx, job, inject.GitHubInput(*token, value))
	}
	if err != nil {
		return fmt.Errorf("writing the GitHub token into its volume: %w", err)
	}
	return creds.store.SetPending(w.id, config.PendingGitHub, false)
}

// syncEnv brings the variables' file in line with the records. With
// nothing to hand out the volume is written only if it may still hold
// something (the mark).
func (w volumeWriter) syncEnv(ctx context.Context, creds *credentials) error {
	variables, err := creds.variables()
	if err != nil {
		return err
	}
	if len(variables) == 0 && !creds.store.Pending(w.id, config.PendingEnv) {
		return nil
	}
	job, err := w.job(ctx, inject.TokensWrite)
	if err == nil {
		err = w.eng.WriteVolume(ctx, job, inject.EnvInput(variables))
	}
	if err != nil {
		return fmt.Errorf("writing the variables into their volume: %w", err)
	}
	return creds.store.SetPending(w.id, config.PendingEnv, false)
}

// sync is both, before an environment starts. A failure stops the start:
// the environment would otherwise come up with something that was taken
// away on this machine, or without something its user counts on.
func (w volumeWriter) sync(ctx context.Context, creds *credentials) error {
	if err := w.syncGitHub(ctx, creds); err != nil {
		return err
	}
	return w.syncEnv(ctx, creds)
}

// apply brings one of the environment's volumes in line right after the
// records changed. Its error is not the command's: the records are saved,
// and the next bh up applies them. When what was taken away may still be in
// the volume, a mark makes sure that bh up clears it.
func (a *app) apply(ctx context.Context, creds *credentials, part func(volumeWriter, context.Context, *credentials) error, mark string) error {
	if creds.env.Archived != "" {
		return nil // no volumes; bh up after bh unarchive writes them
	}
	err := func() error {
		writer, disconnect, err := a.volumeWriter(ctx, creds.store, creds.env.ID)
		if err != nil {
			return err
		}
		defer disconnect()
		// Marked before the attempt: whatever the write leaves behind, the
		// next start goes over it again.
		if err := creds.store.SetPending(creds.env.ID, mark, true); err != nil {
			return err
		}
		return part(writer, ctx, creds)
	}()
	if err != nil {
		if markErr := creds.store.SetPending(creds.env.ID, mark, true); markErr != nil {
			return fmt.Errorf("%s (and that could not be noted for the next start: %v)", oneLine(err), markErr)
		}
	}
	return err
}

func (a *app) applyEnv(ctx context.Context, creds *credentials) error {
	return a.apply(ctx, creds, volumeWriter.syncEnv, config.PendingEnv)
}

func (a *app) applyGitHub(ctx context.Context, creds *credentials) error {
	return a.apply(ctx, creds, volumeWriter.syncGitHub, config.PendingGitHub)
}

// credentialLeftovers deals with what an environment that is being
// destroyed was handed, before its records go: its tokens are revoked, and
// every value leaves the place it is kept in (the credential store would
// hold them for good). What comes back are warnings; none of it stops the
// destruction.
func (a *app) credentialLeftovers(ctx context.Context, store config.Store, id, name string) []string {
	creds, err := a.credentialsOf(store, config.Environment{ID: id, Name: name, Type: config.TypeLocal}, false)
	if err != nil {
		return []string{fmt.Sprintf("the token records could not be read, so no token was revoked — check the services by hand: %v", err)}
	}
	// The values of the secrets first: revoking forgets the tokens' own.
	secrets := &credentials{store: creds.store, env: creds.env, vault: creds.vault, Secrets: creds.Secrets}
	var warnings []string
	if err := secrets.forget(); err != nil {
		warnings = append(warnings, fmt.Sprintf("the values of the secrets could not be removed from where they are kept: %v", err))
	}
	_, revokeWarnings := a.revokeTokens(ctx, creds, false)
	return append(warnings, revokeWarnings...)
}

// readLine reads one line of what the user types.
func readLine(in io.Reader) (string, error) {
	return bufio.NewReader(in).ReadString('\n')
}

// notYet words a change that is saved but not in the environment.
func notYet(env config.Environment, err error) string {
	return fmt.Sprintf("saved, but not yet in the environment (%s) — bh up %s applies it", oneLine(err), env.Name)
}

// newShells says when a change is seen inside.
const newShells = "new shells see it (running shells keep what they had)"

// readSecretValue reads a value the user pastes: hidden when typed at a
// terminal, from standard input when piped.
func (a *app) readSecretValue(prompt string) (string, error) {
	if a.isTerminal() {
		return a.readSecret(prompt)
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
