package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/vault"
)

const secretUsage = `usage: bh secret <subcommand>

  secret list <env>             the names of the secrets of <env>
  secret set <env> <NAME>       set NAME: reads the value (hidden; or from
                                stdin). An existing NAME is replaced
  secret remove <env> <NAME>    take NAME out of <env>
  secret store [keychain|file]  where this machine keeps the values of
                                secrets and tokens; without an argument,
                                say where

  A secret is a value you have — an API key, a password — that the
  environment gets as an environment variable. Its value is never shown,
  and it is not copied by bh create --from nor part of a snapshot. For a
  value that is no secret use bh var; for a credential a service issues for
  the environment (GitHub, Hugging Face), bh token.

  Values are kept in the credential store of the operating system (the
  macOS keychain, the Windows Credential Manager, the Secret Service on
  Linux), which keeps them out of files and backups. It does not hide them
  from programs running as you, and everything inside the environment can
  read them: that is what handing them over means.`

// secretChanged is what setting or removing a secret came to.
type secretChanged struct {
	Env      config.Environment
	Replaced bool
	SyncErr  error
}

// setSecret gives the environment a secret, in place of one of the same
// name if there is one.
func (a *app) setSecret(ctx context.Context, ref, name, value string) (secretChanged, error) {
	store, err := a.store()
	if err != nil {
		return secretChanged{}, err
	}
	env, err := handedTo(store, ref)
	if err != nil {
		return secretChanged{}, err
	}
	result := secretChanged{Env: env}
	if err := config.CheckName(name); err != nil {
		return result, fmt.Errorf("%s: %w", name, err)
	}
	if _, isVar := env.Vars[name]; isVar {
		return result, fmt.Errorf("%s is a variable of %q — take it out first (bh var unset %s %s)", name, env.Name, env.Name, name)
	}
	// A pasted value ends in a line break.
	value = strings.TrimSpace(value)
	if err := config.CheckValue(value); err != nil {
		return result, err
	}
	creds, err := a.credentialsOf(store, env, true)
	if err != nil {
		return result, err
	}
	if err := creds.vault.Set(config.SecretKey(name), value); err != nil {
		return result, fmt.Errorf("keeping the secret: %w", err)
	}
	at := slices.IndexFunc(creds.Secrets, func(s config.Secret) bool { return s.Name == name })
	entry := config.Secret{Name: name, AddedAt: config.Timestamp(a.now())}
	if result.Replaced = at >= 0; result.Replaced {
		// A replaced secret keeps its place in the list.
		creds.Secrets = slices.Clone(creds.Secrets)
		creds.Secrets[at] = entry
	} else {
		creds.Secrets = append(creds.Secrets, entry)
	}
	if err := store.SaveSecrets(env.ID, creds.Secrets); err != nil {
		return result, err
	}
	result.SyncErr = a.applyEnv(ctx, creds)
	return result, nil
}

// removeSecret takes a secret out of the environment and forgets its value.
func (a *app) removeSecret(ctx context.Context, ref, name string) (secretChanged, error) {
	store, err := a.store()
	if err != nil {
		return secretChanged{}, err
	}
	env, err := handedTo(store, ref)
	if err != nil {
		return secretChanged{}, err
	}
	result := secretChanged{Env: env}
	creds, err := a.credentialsOf(store, env, false)
	if err != nil {
		return result, err
	}
	at := slices.IndexFunc(creds.Secrets, func(s config.Secret) bool { return s.Name == name })
	if at < 0 {
		if service := config.TokenVariables[name]; service != "" {
			return result, fmt.Errorf("%s is the %s token's — bh token remove %s %s", name, service, env.Name, service)
		}
		return result, fmt.Errorf("%q has no secret %s", env.Name, name)
	}
	creds.Secrets = slices.Delete(slices.Clone(creds.Secrets), at, at+1)
	if err := store.SaveSecrets(env.ID, creds.Secrets); err != nil {
		return result, err
	}
	if err := creds.vault.Delete(config.SecretKey(name)); err != nil {
		a.warn([]string{fmt.Sprintf("the value of %s could not be removed from where it is kept (%s)", name, oneLine(err))})
	}
	result.SyncErr = a.applyEnv(ctx, creds)
	return result, nil
}

// moveSecretStore changes where this machine keeps values, and takes every
// value along: into the new place first, out of the old one after.
func (a *app) moveSecretStore(to string) (moved int, err error) {
	store, cfg, err := a.load()
	if err != nil {
		return 0, err
	}
	if forced := a.getenv("BH_SECRET_STORE"); forced != "" {
		return 0, fmt.Errorf("BH_SECRET_STORE is set (%s): it decides, not the setting", forced)
	}
	from, err := store.SecretStore()
	if err != nil {
		return 0, err
	}
	if from == to {
		return 0, nil
	}
	if to == vault.KindOS {
		owner, err := store.Init()
		if err != nil {
			return 0, err
		}
		if err := a.probeVault(owner + "/"); err != nil {
			return 0, fmt.Errorf("the credential store of this machine cannot be used (%s) — nothing was changed", oneLine(err))
		}
	}
	if from == "" {
		// Nothing was decided, so nothing is stored anywhere yet: the
		// choice is all there is to do. (Records of earlier builds still
		// hold their values themselves; they go to the chosen place when
		// they are taken over.)
		return 0, store.SetSecretStore(to)
	}
	type item struct {
		source, target vault.Store
		key            string
	}
	var items []item
	for _, env := range cfg.Envs {
		if !env.Local() {
			continue
		}
		// Records of earlier builds are taken over into the place that is
		// being left, then moved with the rest.
		if _, err := a.credentialsOf(store, env, false); err != nil {
			return 0, err
		}
		tokens, err := store.LoadTokens(env.ID)
		if err != nil {
			return 0, err
		}
		secrets, err := store.LoadSecrets(env.ID)
		if err != nil {
			return 0, err
		}
		source, err := a.vaultOf(store, env.ID, from)
		if err != nil {
			return 0, err
		}
		target, err := a.vaultOf(store, env.ID, to)
		if err != nil {
			return 0, err
		}
		for _, token := range tokens {
			items = append(items, item{source, target, config.TokenKey(token.Service)})
		}
		for _, secret := range secrets {
			items = append(items, item{source, target, config.SecretKey(secret.Name)})
		}
	}
	for _, it := range items {
		value, err := it.source.Get(it.key)
		if errors.Is(err, vault.ErrNotFound) {
			continue
		}
		if err == nil {
			err = it.target.Set(it.key, value)
		}
		if err != nil {
			return moved, fmt.Errorf("moving %s: %w\n(nothing was removed from where the values were; the setting is unchanged)", it.key, err)
		}
		moved++
	}
	if err := store.SetSecretStore(to); err != nil {
		return moved, err
	}
	for _, it := range items {
		if err := it.source.Delete(it.key); err != nil {
			a.warn([]string{fmt.Sprintf("%s is in its new place, but could not be removed from the old one (%s)", it.key, oneLine(err))})
		}
	}
	return moved, nil
}

func storeName(kind string) string {
	switch kind {
	case vault.KindOS:
		return "the credential store of the operating system (keychain)"
	case vault.KindFile:
		return "a file in each environment's directory, readable only by you (file)"
	}
	return "not decided yet — the credential store, if this machine has one, when the first value is set"
}

func (a *app) secret(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("%s", secretUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		if len(rest) != 1 {
			return usagef("usage: bh secret list <env>")
		}
		store, err := a.store()
		if err != nil {
			return err
		}
		env, err := holderOf(store, rest[0])
		if err != nil {
			return err
		}
		creds, err := a.credentialsOf(store, env, false)
		if err != nil {
			return err
		}
		if len(creds.Secrets) == 0 {
			fmt.Fprintf(a.stdout, "%q has no secrets (bh secret set %s <NAME>)\n", env.Name, env.Name)
			return nil
		}
		for _, secret := range creds.Secrets {
			fmt.Fprintln(a.stdout, secret.Name)
		}
		return nil

	case "set":
		if len(rest) != 2 {
			return usagef("usage: bh secret set <env> <NAME>")
		}
		store, err := a.store()
		if err != nil {
			return err
		}
		// Checked before the value is asked for: being turned down for the
		// name after pasting means pasting twice.
		env, err := handedTo(store, rest[0])
		if err != nil {
			return err
		}
		if err := config.CheckName(rest[1]); err != nil {
			return fmt.Errorf("%s: %w", rest[1], err)
		}
		value, err := a.readSecretValue(fmt.Sprintf("Value of %s (input is hidden): ", rest[1]))
		if err != nil {
			return err
		}
		result, err := a.setSecret(ctx, env.Name, rest[1], value)
		if err != nil {
			return err
		}
		verb := "Set"
		if result.Replaced {
			verb = "Replaced"
		}
		a.reportChange(fmt.Sprintf("%s the secret %s for %q", verb, rest[1], result.Env.Name), result.Env, result.SyncErr)
		return nil

	case "remove":
		if len(rest) != 2 {
			return usagef("usage: bh secret remove <env> <NAME>")
		}
		result, err := a.removeSecret(ctx, rest[0], rest[1])
		if err != nil {
			return err
		}
		a.reportChange(fmt.Sprintf("Took the secret %s out of %q", rest[1], result.Env.Name), result.Env, result.SyncErr)
		return nil

	case "store":
		store, err := a.store()
		if err != nil {
			return err
		}
		if len(rest) == 0 {
			kind, err := a.secretStoreKind(store, false)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Values of secrets and tokens are kept in %s\n", storeName(kind))
			return nil
		}
		if len(rest) != 1 || (rest[0] != vault.KindOS && rest[0] != vault.KindFile) {
			return usagef("usage: bh secret store [%s|%s]", vault.KindOS, vault.KindFile)
		}
		moved, err := a.moveSecretStore(rest[0])
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Values of secrets and tokens are kept in %s", storeName(rest[0]))
		if moved > 0 {
			fmt.Fprintf(a.stdout, " — %d moved there", moved)
		}
		fmt.Fprintln(a.stdout)
		return nil
	}
	return usagef("%s", secretUsage)
}
