package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/ys319/bulkhead/internal/config"
)

const varUsage = `usage: bh var <subcommand>

  var list <env>                the variables of <env>, with their values
  var set <env> <KEY> <VALUE>   set KEY (an existing KEY is replaced)
  var unset <env> <KEY>         take KEY out

  A variable is an environment variable that is no secret: it is kept with
  the settings (and copied by bh create --from), and its value is shown.
  For a value that must not be shown, use bh secret; for a credential a
  service issues, bh token. New shells see a change; running shells keep
  what they had. A value is one line of text.`

// A variable is the plainest of the three things an environment is handed
// (see creds.go): a name and a value in env.json.

// varChanged is what setting or unsetting a variable came to.
type varChanged struct {
	Env      config.Environment
	Replaced bool
	// SyncErr: the settings are saved but the environment's volume is not
	// written; the next bh up does it.
	SyncErr error
}

// changeVar sets key to value, or takes key out when unset is true.
func (a *app) changeVar(ctx context.Context, ref, key, value string, unset bool) (varChanged, error) {
	store, err := a.store()
	if err != nil {
		return varChanged{}, err
	}
	env, err := holderOf(store, ref)
	if err != nil {
		return varChanged{}, err
	}
	result := varChanged{Env: env}
	_, had := env.Vars[key]
	vars := maps.Clone(env.Vars)
	if unset {
		if !had {
			return result, fmt.Errorf("%q has no variable %s", env.Name, key)
		}
		delete(vars, key)
	} else {
		if err := config.CheckName(key); err != nil {
			return result, fmt.Errorf("%s: %w", key, err)
		}
		if err := config.CheckValue(value); err != nil {
			return result, err
		}
		// One name, one route: a secret of that name would come out of the
		// same file inside, and one of the two would silently win.
		secrets, err := store.LoadSecrets(env.ID)
		if err != nil {
			return result, err
		}
		if slices.ContainsFunc(secrets, func(s config.Secret) bool { return s.Name == key }) {
			return result, fmt.Errorf("%s is a secret of %q — remove it first (bh secret remove %s %s)", key, env.Name, env.Name, key)
		}
		if vars == nil {
			vars = map[string]string{}
		}
		vars[key] = value
		result.Replaced = had
	}
	env.Vars = vars
	if err := store.SaveEnvironment(env); err != nil {
		return result, err
	}
	result.Env = env
	creds, err := a.credentialsOf(store, env, false)
	if err != nil {
		result.SyncErr = err
		return result, nil
	}
	result.SyncErr = a.applyEnv(ctx, creds)
	return result, nil
}

func (a *app) variable(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("%s", varUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		if len(rest) != 1 {
			return usagef("usage: bh var list <env>")
		}
		store, err := a.store()
		if err != nil {
			return err
		}
		env, err := holderOf(store, rest[0])
		if err != nil {
			return err
		}
		if len(env.Vars) == 0 {
			fmt.Fprintf(a.stdout, "%q has no variables (bh var set %s <KEY> <VALUE>)\n", env.Name, env.Name)
			return nil
		}
		names := make([]string, 0, len(env.Vars))
		for name := range env.Vars {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(a.stdout, "%s=%s\n", name, env.Vars[name])
		}
		return nil

	case "set":
		if len(rest) != 3 {
			return usagef("usage: bh var set <env> <KEY> <VALUE>")
		}
		result, err := a.changeVar(ctx, rest[0], rest[1], rest[2], false)
		if err != nil {
			return err
		}
		verb := "Set"
		if result.Replaced {
			verb = "Replaced"
		}
		a.reportChange(fmt.Sprintf("%s %s for %q", verb, rest[1], result.Env.Name), result.Env, result.SyncErr)
		return nil

	case "unset":
		if len(rest) != 2 {
			return usagef("usage: bh var unset <env> <KEY>")
		}
		result, err := a.changeVar(ctx, rest[0], rest[1], "", true)
		if err != nil {
			return err
		}
		a.reportChange(fmt.Sprintf("Took %s out of %q", rest[1], result.Env.Name), result.Env, result.SyncErr)
		return nil
	}
	return usagef("%s", varUsage)
}

// reportChange prints what was done and when it is seen inside, or — when
// the volume could not be written — that the next start applies it.
func (a *app) reportChange(done string, env config.Environment, syncErr error) {
	if syncErr != nil {
		fmt.Fprintln(a.stdout, done)
		a.warn([]string{notYet(env, syncErr)})
		return
	}
	fmt.Fprintf(a.stdout, "%s — %s\n", done, newShells)
}
