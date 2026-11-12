package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/ops"
	"github.com/ys319/bulkhead/internal/plan"
	"github.com/ys319/bulkhead/internal/sshkey"
)

// migrate takes environments over from the TypeScript version, one at a
// time. What it may and may not do:
//
//   - It never removes, creates over or copies a volume. The volumes keep
//     their names and are used as they are.
//   - In the TypeScript version's directory it changes one thing: the
//     migrated environment's directory moves to environments.migrated/.
//     Nothing there is deleted.
//   - It says what it is about to do and asks before doing it.
func (a *app) migrate(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	yes := flags.Bool("yes", false, "")
	undo := flags.Bool("undo", false, "")
	rest, err := parseFlags(flags, args)
	if err != nil {
		return err
	}

	store, err := a.store()
	if err != nil {
		return err
	}
	legacyDir, err := config.LegacyDir(a.goos, a.getenv)
	if err != nil {
		return fmt.Errorf("cannot locate the TypeScript version's configuration directory: %w", err)
	}
	if filepath.Clean(legacyDir) == filepath.Clean(store.Dir) {
		return fmt.Errorf("bh and the TypeScript version are pointed at the same directory (%s); they need one each", store.Dir)
	}
	legacy := config.LegacyStore{Dir: legacyDir}

	switch {
	case len(rest) == 0 && !*undo:
		return a.migrateList(legacy, store)
	case len(rest) == 1 && *undo:
		return a.migrateUndo(ctx, legacy, store, rest[0], *yes)
	case len(rest) == 1:
		return a.migrateOne(ctx, legacy, store, rest[0], *yes)
	}
	return usagef("usage: bh migrate [<name>] [--yes] | bh migrate --undo <name> [--yes]")
}

// assessment is what migrating one environment would involve.
type assessment struct {
	env      config.Environment
	blockers []string // reasons it cannot be migrated as things are
	notes    []string // what will not come along
	keys     []config.KeyEntry
	// resume: bh already has this environment's settings — an earlier run
	// was interrupted after writing them. The rest is safe to repeat.
	resume bool
}

func assess(legacy config.LegacyStore, store config.Store, name string) (assessment, error) {
	env, err := legacy.Load(name)
	if err != nil {
		return assessment{}, err
	}
	result := assessment{env: env}
	block := func(format string, args ...any) {
		result.blockers = append(result.blockers, fmt.Sprintf(format, args...))
	}
	note := func(format string, args ...any) { result.notes = append(result.notes, fmt.Sprintf(format, args...)) }

	extras := legacy.Extras(name)
	if env.Local() {
		if extras.Override {
			block("it has a compose.override.yaml, and bh has nothing that takes its place (bh does not use Compose)")
		}
		if extras.DevDockerfile {
			block("it has a dev.Dockerfile, which this build does not support yet")
		}
		if env.GPU != "off" {
			block("it asks for a GPU (gpu: %s), which this build does not support yet", env.GPU)
		}
		if extras.GitHubToken {
			note("its GitHub token: this build has no secrets yet, so git and gh in the environment will be without it")
		}
		if extras.Tokens {
			note("its other tokens (tokens.json): this build has no secrets yet")
		}
		if extras.InitScript {
			note("init.sh: this build does not run it yet")
		}
		note("the ssh config entry bulkhead-%s (VS Code uses it): this build does not write one yet, and the TypeScript version drops the entry the next time it rewrites its ssh config. bh ssh %s works regardless", name, name)
	}

	// The keys it lets in must come along, or people are locked out.
	registry, err := legacy.LoadKeys()
	if err != nil {
		return assessment{}, err
	}
	byFingerprint := map[string]config.KeyEntry{}
	for _, entry := range registry {
		byFingerprint[entry.Fingerprint] = entry
	}
	for _, ref := range env.AuthorizedKeys {
		entry, found := byFingerprint[ref]
		if !found {
			block("it refers to the key %s, which is not in the TypeScript version's keys.json", ref)
			continue
		}
		result.keys = append(result.keys, entry)
	}

	cfg, err := store.Load()
	if err != nil {
		return assessment{}, err
	}
	for _, other := range cfg.Envs {
		switch {
		case other.ID == name && other.Name == name:
			result.resume = true
		case other.ID == name:
			block("bh already has an environment with the id %q (named %q)", name, other.Name)
		case other.Name == name:
			block("bh already has an environment named %q", name)
		case env.Local() && other.Local() && other.SSHPort == env.SSHPort:
			block("its ssh port %d is already used by %q in bh", env.SSHPort, other.Name)
		}
	}
	for _, broken := range cfg.Broken {
		if broken.ID == name {
			block("bh already has an environment with the id %q, and its settings cannot be read", name)
		}
	}
	if legacy.Retired(name) {
		block("%s already exists (from an earlier migration of the same name); move it away first", legacy.RetiredDir(name))
	}
	return result, nil
}

func (a *app) migrateList(legacy config.LegacyStore, store config.Store) error {
	names, err := legacy.Names()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Fprintf(a.stdout, "The TypeScript version has no environments (looked in %s)\n", legacy.Dir)
		return nil
	}
	fmt.Fprintf(a.stdout, "Environments of the TypeScript version (%s):\n\n", legacy.Dir)
	table := tabwriter.NewWriter(a.stdout, 2, 0, 2, ' ', 0)
	fmt.Fprintln(table, "  NAME\tSSH\tMIGRATION")
	for _, name := range names {
		result, err := assess(legacy, store, name)
		switch {
		case err != nil:
			fmt.Fprintf(table, "  %s\t-\tcannot be read: %s\n", name, firstLine(err.Error()))
		case len(result.blockers) > 0:
			fmt.Fprintf(table, "  %s\t%s\tblocked: %s\n", name, sshAddress(result.env), result.blockers[0])
		default:
			fmt.Fprintf(table, "  %s\t%s\tready\n", name, sshAddress(result.env))
		}
	}
	if err := table.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, "\nTake one over with: bh migrate <name>   (it shows what it will do and asks first)")
	return nil
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// confirm asks a yes/no question; anything but y / yes is no.
func (a *app) confirm(question string) bool {
	fmt.Fprintf(a.stderr, "%s [y/N] ", question)
	answer, _ := bufio.NewReader(a.stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	}
	return false
}

func (a *app) migrateOne(ctx context.Context, legacy config.LegacyStore, store config.Store, name string, yes bool) error {
	result, err := assess(legacy, store, name)
	if err != nil {
		return err
	}
	env := result.env
	if len(result.blockers) > 0 {
		return fmt.Errorf("%q cannot be migrated as it is:\n  - %s\n(nothing was changed; it stays with the TypeScript version)",
			name, strings.Join(result.blockers, "\n  - "))
	}

	var eng dockerEngine
	if env.Local() {
		var disconnect func()
		eng, disconnect, err = a.dial(a.getenv)
		if err != nil {
			return err
		}
		defer disconnect()
	}

	// Say what will happen, with what is there now.
	out := a.stdout
	fmt.Fprintf(out, "Migrating %q\n  from %s\n  to   %s\n\n", name, legacy.Dir, store.Dir)
	if result.resume {
		fmt.Fprintln(out, "  (bh already has its settings: an earlier run was interrupted. Finishing it.)")
	}
	fmt.Fprintf(out, "  settings    carried over (id %s, ssh %s)\n", env.ID, sshAddress(env))
	if env.Local() {
		fmt.Fprintln(out, "  volumes     used as they are — not copied, not removed:")
		for _, key := range plan.VolumeKeys {
			volume := plan.Volume(env.ID, key)
			exists, err := eng.VolumeExists(ctx, volume)
			if err != nil {
				return err
			}
			state := "exists"
			if !exists {
				state = "does not exist yet; will be created empty"
			}
			fmt.Fprintf(out, "                %s (%s)\n", volume, state)
		}
		running := false
		for _, old := range []string{plan.LegacyHolderName(name), plan.LegacyDevName(name)} {
			found, err := eng.Container(ctx, old)
			if err != nil {
				return err
			}
			running = running || (found != nil && found.Running())
		}
		fmt.Fprintf(out, "  containers  %s, %s\n", plan.LegacyHolderName(name), plan.LegacyDevName(name))
		fmt.Fprintf(out, "              are replaced by %s, %s\n", plan.HolderName(env.ID), plan.DevName(env.ID))
		if running {
			fmt.Fprintln(out, "              it is running: it stops for a moment and comes back up in the new")
			fmt.Fprintln(out, "              containers (open ssh sessions end). If they do not come up, the old")
			fmt.Fprintln(out, "              ones are started again.")
		} else {
			fmt.Fprintln(out, "              it is stopped and stays stopped")
		}
		fmt.Fprintf(out, "  network     %s is replaced by %s\n", plan.LegacyNetwork(name), plan.Network(env.ID))
		if env.DockerSocket {
			fmt.Fprintf(out, "  warning     %s\n", socketWarning)
		}
	}
	fmt.Fprintf(out, "  afterwards  the old settings move to %s\n              (kept; the TypeScript version no longer lists the environment)\n", legacy.RetiredDir(name))
	if len(result.notes) > 0 {
		fmt.Fprintln(out, "\n  not coming along:")
		for _, note := range result.notes {
			fmt.Fprintf(out, "    - %s\n", note)
		}
	}
	fmt.Fprintln(out)
	if !yes && !a.confirm("Proceed?") {
		return errors.New("not confirmed — nothing was changed")
	}

	// The keys first: the containers are created from them.
	if _, err := store.Init(); err != nil {
		return err
	}
	extraKey, err := adoptMachineKey(legacy, store)
	if err != nil {
		return err
	}
	keys := result.keys
	if extraKey != nil && env.Local() {
		keys = append(keys, *extraKey)
		env.AuthorizedKeys = append(env.AuthorizedKeys, extraKey.Fingerprint)
	}
	if err := store.AddKeys(keys); err != nil {
		return err
	}

	adoption := ops.Adoption{}
	if env.Local() {
		planned, target, err := containersFor(store, env)
		if err != nil {
			return err
		}
		if err := a.realize(ctx, eng, &planned, &target); err != nil {
			return err
		}
		if adoption, err = (ops.Runner{Engine: eng}).Adopt(ctx, planned, target); err != nil {
			return err
		}
	}

	// Docker is done; now the two directories. If either step fails, running
	// the command again finishes the job.
	if err := store.SaveEnvironment(env); err != nil {
		return fmt.Errorf("the containers are replaced, but writing the settings failed: %w\n(run bh migrate %s again)", err, name)
	}
	if err := legacy.Retire(name); err != nil {
		return fmt.Errorf("bh has taken %q over, but moving its old settings away failed: %w\n(run bh migrate %s again; until then do not start it with the TypeScript version)", name, err, name)
	}
	a.syncSSH(store)

	fmt.Fprintf(out, "Migrated %q\n", name)
	switch {
	case !env.Local():
	case adoption.Started:
		fmt.Fprintf(out, "  it is up in the new containers — ssh: %s (bh ssh %s)\n", sshAddress(env), name)
	default:
		fmt.Fprintf(out, "  it is stopped — start it with bh up %s\n", name)
	}
	fmt.Fprintf(out, "  old settings: %s\n", legacy.RetiredDir(name))
	for _, leftover := range adoption.Leftovers {
		fmt.Fprintf(out, "  left behind (remove by hand): %s\n", leftover)
	}
	if env.Local() {
		// Volumes only the TypeScript version used. bh removes no volume
		// here, not even these.
		var unused []string
		for _, key := range []string{"github", "tokens"} {
			if exists, _ := eng.VolumeExists(ctx, plan.Volume(env.ID, key)); exists {
				unused = append(unused, plan.Volume(env.ID, key))
			}
		}
		if len(unused) > 0 {
			fmt.Fprintf(out, "  no longer used, left in place: the volumes %s\n", strings.Join(unused, ", "))
		}
	}
	fmt.Fprintf(out, "  to hand it back to the TypeScript version: bh migrate --undo %s\n", name)
	return nil
}

// adoptMachineKey makes sure the key the TypeScript version logged in with
// keeps working. If bh has no machine key yet, it takes that one over (a
// copy; the original stays). If bh has its own already, the old public key
// is returned to be registered and let into the environment.
func adoptMachineKey(legacy config.LegacyStore, store config.Store) (*config.KeyEntry, error) {
	oldPublic, err := os.ReadFile(legacy.MachineKey() + ".pub")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil // it never had one; bh makes its own
	}
	if err != nil {
		return nil, err
	}
	oldLine := strings.TrimSpace(string(oldPublic))
	oldPrint, err := sshkey.Fingerprint(oldLine)
	if err != nil {
		return nil, fmt.Errorf("%s.pub: %w", legacy.MachineKey(), err)
	}

	ownPublic, err := os.ReadFile(store.MachineKey() + ".pub")
	if err == nil {
		ownPrint, err := sshkey.Fingerprint(string(ownPublic))
		if err != nil {
			return nil, fmt.Errorf("%s.pub: %w", store.MachineKey(), err)
		}
		if ownPrint == oldPrint {
			return nil, nil
		}
		return &config.KeyEntry{Fingerprint: oldPrint, PublicKey: oldLine, Label: "machine key of the TypeScript version"}, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if _, err := os.Stat(store.MachineKey()); err == nil {
		// A private half without the public one: bh restores the public half
		// itself; just make sure the old key gets in too.
		return &config.KeyEntry{Fingerprint: oldPrint, PublicKey: oldLine, Label: "machine key of the TypeScript version"}, nil
	}

	private, err := os.ReadFile(legacy.MachineKey())
	if err != nil {
		return nil, fmt.Errorf("copying the machine key: %w", err)
	}
	// O_EXCL: never over a key that is already there.
	file, err := os.OpenFile(store.MachineKey(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := file.Write(private); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	return nil, os.WriteFile(store.MachineKey()+".pub", oldPublic, 0o644)
}

// migrateUndo hands a migrated environment back to the TypeScript version.
// It is the way to leave bh without `bh destroy`, which removes the data.
func (a *app) migrateUndo(ctx context.Context, legacy config.LegacyStore, store config.Store, name string, yes bool) error {
	cfg, err := store.Load()
	if err != nil {
		return err
	}
	env, err := cfg.Find(name)
	if err != nil {
		return err
	}
	id := env.ID
	if !legacy.Retired(id) {
		return fmt.Errorf("%q did not come from the TypeScript version (there is no %s)", name, legacy.RetiredDir(id))
	}
	if _, err := os.Stat(legacy.EnvDir(id)); err == nil {
		return fmt.Errorf("the TypeScript version has an environment named %q again (%s); not putting the old one over it", id, legacy.EnvDir(id))
	}

	out := a.stdout
	fmt.Fprintf(out, "Handing %q back to the TypeScript version\n\n", name)
	if env.Local() {
		fmt.Fprintf(out, "  containers  %s, %s are stopped and removed\n", plan.HolderName(id), plan.DevName(id))
		fmt.Fprintf(out, "  network     %s is removed\n", plan.Network(id))
		fmt.Fprintln(out, "  volumes     kept as they are — nothing is removed")
	}
	fmt.Fprintf(out, "  settings    %s moves back to %s;\n              bh's own settings for it are removed (changes made in bh are lost)\n\n", legacy.RetiredDir(id), legacy.EnvDir(id))
	if !yes && !a.confirm("Proceed?") {
		return errors.New("not confirmed — nothing was changed")
	}

	if env.Local() {
		eng, disconnect, err := a.dial(a.getenv)
		if err != nil {
			return err
		}
		defer disconnect()
		if err := (ops.Runner{Engine: eng}).Release(ctx, id); err != nil {
			return err
		}
	}
	if err := legacy.Restore(id); err != nil {
		return err
	}
	if err := store.RemoveEnvironment(id); err != nil {
		return err
	}
	a.syncSSH(store)
	fmt.Fprintf(out, "Handed %q back\n", name)
	if env.Local() {
		fmt.Fprintf(out, "  start it with the TypeScript version's bh (bh up %s there).\n", id)
		fmt.Fprintln(out, "  Until then no container refers to its volumes: do not run `docker volume prune -a`.")
	}
	return nil
}
