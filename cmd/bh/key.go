package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/ys319/bulkhead/internal/config"
)

const keyUsage = `usage: bh key <subcommand>

  key show                  print this machine's public key (made if missing)
  key list [env]            the registered keys, or the keys attached to <env>
  key add [--label <name>] [key...]
                            register a public key (read from stdin when not
                            given; the label defaults to the key's comment)
  key rename <key> <label>  change the label shown for a registered key
  key remove <key> [--yes]  remove from the registry (detaches it everywhere)
  key attach <env> <key>    let the key log in to <env> (applies at bh up)
  key detach <env> <key>    take that back (applies at bh up)

  <key> is a label, or a fingerprint or the start of one`

// machineKeyComment marks the machine key in an authorized_keys file.
const machineKeyComment = "bulkhead"

// The key registry: public keys, other than this machine's, that may log in
// to environments. An environment refers to registered keys by fingerprint.

// registerKey adds a pasted public key to the registry.
func registerKey(store config.Store, input, label string, labelGiven bool) (config.KeyEntry, error) {
	publicKey, err := config.ParsePublicKey(input)
	if err != nil {
		return config.KeyEntry{}, err
	}
	entry, err := config.NewKeyEntry(publicKey, label, labelGiven)
	if err != nil {
		return config.KeyEntry{}, err
	}
	registry, err := store.LoadKeys()
	if err != nil {
		return config.KeyEntry{}, err
	}
	for _, existing := range registry {
		if existing.Fingerprint == entry.Fingerprint {
			return config.KeyEntry{}, fmt.Errorf("already registered as %q — %s", existing.DisplayName(), existing.Fingerprint)
		}
	}
	return entry, store.SaveKeys(append(registry, entry))
}

// renameKey changes the label of a registered key.
func renameKey(store config.Store, ref, label string) (config.KeyEntry, error) {
	registry, err := store.LoadKeys()
	if err != nil {
		return config.KeyEntry{}, err
	}
	target, err := config.FindKey(registry, ref)
	if err != nil {
		return config.KeyEntry{}, err
	}
	if target.Label, err = config.NormalizeLabel(label); err != nil {
		return config.KeyEntry{}, err
	}
	for i := range registry {
		if registry[i].Fingerprint == target.Fingerprint {
			registry[i] = target
		}
	}
	return target, store.SaveKeys(registry)
}

// removeKey takes a key out of the registry, after detaching it from every
// environment: an environment that refers to an unregistered key does not
// start. The environments go first, so that an interruption leaves a
// registered key that is merely attached to fewer of them.
func removeKey(store config.Store, fingerprint string) (detachedFrom []string, err error) {
	cfg, err := store.Load()
	if err != nil {
		return nil, err
	}
	for _, env := range cfg.Envs {
		if !env.Local() || !slices.Contains(env.AuthorizedKeys, fingerprint) {
			continue
		}
		env.AuthorizedKeys = slices.DeleteFunc(slices.Clone(env.AuthorizedKeys), func(fp string) bool { return fp == fingerprint })
		if err := store.SaveEnvironment(env); err != nil {
			return detachedFrom, err
		}
		detachedFrom = append(detachedFrom, env.Name)
	}
	registry, err := store.LoadKeys()
	if err != nil {
		return detachedFrom, err
	}
	registry = slices.DeleteFunc(registry, func(entry config.KeyEntry) bool { return entry.Fingerprint == fingerprint })
	return detachedFrom, store.SaveKeys(registry)
}

// keyHolder is the local environment a key is attached to or detached from.
func keyHolder(cfg *config.Config, ref string) (config.Environment, error) {
	env, err := cfg.Find(ref)
	if err != nil {
		return env, err
	}
	if !env.Local() {
		return env, fmt.Errorf("%q is a remote environment — register keys on its own machine", env.Name)
	}
	return env, nil
}

// attachKey lets a registered key log in to the environment.
func attachKey(store config.Store, envRef, keyRef string) (config.Environment, config.KeyEntry, error) {
	cfg, err := store.Load()
	if err != nil {
		return config.Environment{}, config.KeyEntry{}, err
	}
	env, err := keyHolder(cfg, envRef)
	if err != nil {
		return env, config.KeyEntry{}, err
	}
	registry, err := store.LoadKeys()
	if err != nil {
		return env, config.KeyEntry{}, err
	}
	entry, err := config.FindKey(registry, keyRef)
	if err != nil {
		return env, entry, err
	}
	if slices.Contains(env.AuthorizedKeys, entry.Fingerprint) {
		return env, entry, fmt.Errorf("%s is already attached to %q", entry.Fingerprint, env.Name)
	}
	env.AuthorizedKeys = append(slices.Clone(env.AuthorizedKeys), entry.Fingerprint)
	return env, entry, store.SaveEnvironment(env)
}

// detachKey takes a key's access to the environment back. The key may be
// one that is no longer registered.
func detachKey(store config.Store, envRef, keyRef string) (config.Environment, string, error) {
	cfg, err := store.Load()
	if err != nil {
		return config.Environment{}, "", err
	}
	env, err := keyHolder(cfg, envRef)
	if err != nil {
		return env, "", err
	}
	if len(env.AuthorizedKeys) == 0 {
		return env, "", fmt.Errorf("%q has no attached keys", env.Name)
	}
	registry, err := store.LoadKeys()
	if err != nil {
		return env, "", err
	}
	fingerprint, err := config.FindAttached(registry, env, keyRef)
	if err != nil {
		return env, "", err
	}
	env.AuthorizedKeys = slices.DeleteFunc(slices.Clone(env.AuthorizedKeys), func(fp string) bool { return fp == fingerprint })
	return env, fingerprint, store.SaveEnvironment(env)
}

func (a *app) key(_ context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("%s", keyUsage)
	}
	store, err := a.store()
	if err != nil {
		return err
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "show":
		if len(rest) != 0 {
			return usagef("usage: bh key show")
		}
		if _, err := store.Init(); err != nil {
			return err
		}
		publicKey, err := ensureMachineKey(store)
		if err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, publicKey)
		return nil

	case "list":
		return a.keyList(store, rest)

	case "add":
		flags := flag.NewFlagSet("key add", flag.ContinueOnError)
		label := flags.String("label", "", "")
		words, err := parseFlags(flags, rest)
		if err != nil {
			return err
		}
		labelGiven := false
		flags.Visit(func(f *flag.Flag) { labelGiven = labelGiven || f.Name == "label" })
		input := strings.Join(words, " ")
		if len(words) == 0 {
			if a.isTerminal() {
				return usagef("pass the public key as arguments or pipe it in:\n  bh key add < id_ed25519.pub")
			}
			piped, err := io.ReadAll(io.LimitReader(a.stdin, 1<<20))
			if err != nil {
				return err
			}
			input = string(piped)
		}
		if _, err := store.Init(); err != nil {
			return err
		}
		entry, err := registerKey(store, input, *label, labelGiven)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Registered: %s  %s\n", entry.Fingerprint, entry.DisplayName())
		name := entry.Label
		if name == "" || strings.ContainsAny(name, " \t") {
			name = entry.Fingerprint
		}
		fmt.Fprintf(a.stdout, "Attach it with bh key attach <env> %s\n", name)
		return nil

	case "rename":
		if len(rest) != 2 {
			return usagef("usage: bh key rename <key> <label>")
		}
		entry, err := renameKey(store, rest[0], rest[1])
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Renamed: %s  %s\n", entry.Fingerprint, entry.DisplayName())
		return nil

	case "remove":
		flags := flag.NewFlagSet("key remove", flag.ContinueOnError)
		yes := flags.Bool("yes", false, "")
		words, err := parseFlags(flags, rest)
		if err != nil {
			return err
		}
		if len(words) != 1 {
			return usagef("usage: bh key remove <key> [--yes]")
		}
		registry, err := store.LoadKeys()
		if err != nil {
			return err
		}
		entry, err := config.FindKey(registry, words[0])
		if err != nil {
			return err
		}
		cfg, err := store.Load()
		if err != nil {
			return err
		}
		if used := cfg.UsersOf(entry.Fingerprint); len(used) > 0 && !*yes {
			if !a.isTerminal() {
				return fmt.Errorf("key %s is attached to: %s\n(pass --yes to detach it from them and remove it)",
					entry.Fingerprint, strings.Join(used, ", "))
			}
			question := fmt.Sprintf("Key %s is attached to: %s.\nDetach it from them and remove it from the registry?",
				entry.DisplayName(), strings.Join(used, ", "))
			if !a.confirm(question) {
				return errAborted
			}
		}
		detached, err := removeKey(store, entry.Fingerprint)
		if len(detached) > 0 {
			fmt.Fprintf(a.stdout, "Detached from: %s (applies at the next bh up)\n", strings.Join(detached, ", "))
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Removed from the registry: %s\n", entry.Fingerprint)
		return nil

	case "attach":
		if len(rest) != 2 {
			return usagef("usage: bh key attach <env> <key>")
		}
		env, entry, err := attachKey(store, rest[0], rest[1])
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Attached to %q: %s\nApply with bh up %s\n", env.Name, entry.Fingerprint, env.Name)
		return nil

	case "detach":
		if len(rest) != 2 {
			return usagef("usage: bh key detach <env> <key>")
		}
		env, fingerprint, err := detachKey(store, rest[0], rest[1])
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Detached from %q: %s\nApply with bh up %s\n", env.Name, fingerprint, env.Name)
		return nil
	}
	return usagef("%s", keyUsage)
}

func (a *app) keyList(store config.Store, args []string) error {
	if len(args) > 1 {
		return usagef("usage: bh key list [env]")
	}
	registry, err := store.LoadKeys()
	if err != nil {
		return err
	}
	cfg, err := store.Load()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		if len(registry) == 0 {
			fmt.Fprintln(a.stdout, "No keys registered (bh key add)")
			return nil
		}
		for _, entry := range registry {
			where := "(unattached)"
			if used := cfg.UsersOf(entry.Fingerprint); len(used) > 0 {
				where = "→ " + strings.Join(used, ", ")
			}
			fmt.Fprintf(a.stdout, "%s  %s  %s\n", entry.Fingerprint, entry.DisplayName(), where)
		}
		return nil
	}
	env, err := keyHolder(cfg, args[0])
	if err != nil {
		return err
	}
	if len(env.AuthorizedKeys) == 0 {
		fmt.Fprintf(a.stdout, "No keys attached to %q (this machine's key always gets in)\n", env.Name)
		return nil
	}
	names := map[string]string{}
	for _, entry := range registry {
		names[entry.Fingerprint] = entry.DisplayName()
	}
	for _, fingerprint := range env.AuthorizedKeys {
		name, registered := names[fingerprint]
		if !registered {
			name = "(missing from the registry)"
		}
		fmt.Fprintf(a.stdout, "%s  %s\n", fingerprint, name)
	}
	return nil
}
