package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/engine"
	"github.com/ys319/bulkhead/internal/ops"
	"github.com/ys319/bulkhead/internal/plan"
	"github.com/ys319/bulkhead/internal/snapshot"
	"github.com/ys319/bulkhead/internal/tui"
)

// A snapshot is the data of an environment in one file: its workspace and
// its home, without what can be had again (plan.HomeLeftOut). Snapshots
// live in the configuration directory, by environment; bh export writes
// the same file anywhere. Four things are done with one:
//
//	rollback   put the data of an environment back to it
//	archive    take one, then give up the volumes (the environment stays
//	           in the list, asleep)
//	unarchive  the way back from that
//	import     make a new environment from the file
//
// rollback and archive delete data, each after the snapshot it relies on
// has been read back whole.

const snapshotUsage = `usage: bh snapshot <subcommand>

  snapshot create <env> [--note <text>]
                            take a snapshot of the stopped environment
  snapshot list [<env>]     the snapshots (of one environment, or of all)
  snapshot remove <env> <snapshot>

  A snapshot holds the workspace and the home directory. It leaves out the
  nix store and what points into it, caches and the editor's server: after
  bh unarchive or bh import, init.sh runs again and installs the tools.
  It holds whatever is in the home directory — logins, keys, history: keep
  the files as private as the environment.

  See also: bh rollback, bh export, bh import, bh archive, bh unarchive.`

// snapshotName is the file name of a snapshot taken at that moment.
func snapshotName(at time.Time) string {
	return at.UTC().Format("2006-01-02T150405Z") + snapshot.Extension
}

// archivedError turns down what an archived environment cannot do.
func archivedError(env config.Environment) error {
	return fmt.Errorf("%q is archived — bh unarchive %s brings it back", env.Name, env.Name)
}

// snapshotInfo is one snapshot file.
type snapshotInfo struct {
	Name     string // the file's name
	Path     string
	Size     int64
	Manifest snapshot.Manifest
	// Err is why the file could not be read as a snapshot.
	Err error
}

// listSnapshots are the snapshots in the environment's directory, oldest
// first (the names sort by time).
func listSnapshots(store config.Store, id string) ([]snapshotInfo, error) {
	dir := store.SnapshotsDir(id)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var found []snapshotInfo
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), snapshot.Extension) {
			continue // a write in progress or left behind (.partial), or not ours
		}
		info := snapshotInfo{Name: entry.Name(), Path: filepath.Join(dir, entry.Name())}
		if stat, err := entry.Info(); err == nil {
			info.Size = stat.Size()
		}
		info.Manifest, info.Err = snapshot.ReadManifest(info.Path)
		found = append(found, info)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Name < found[j].Name })
	return found, nil
}

// findSnapshot resolves what the user named: "latest", the name of a
// snapshot of the environment (with or without the extension), or the path
// of a snapshot file anywhere.
func findSnapshot(store config.Store, env config.Environment, ref string) (string, error) {
	snapshots, err := listSnapshots(store, env.ID)
	if err != nil {
		return "", err
	}
	if ref == "latest" {
		if len(snapshots) == 0 {
			return "", fmt.Errorf("%q has no snapshots (bh snapshot create %s)", env.Name, env.Name)
		}
		return snapshots[len(snapshots)-1].Path, nil
	}
	for _, candidate := range snapshots {
		if ref == candidate.Name || ref+snapshot.Extension == candidate.Name {
			return candidate.Path, nil
		}
	}
	if info, err := os.Stat(ref); err == nil && !info.IsDir() {
		return ref, nil
	}
	return "", fmt.Errorf("%q has no snapshot %q, and there is no such file (bh snapshot list %s)", env.Name, ref, env.Name)
}

// stoppedLocal checks that the environment's data can be read or replaced
// now: a local environment, not archived, with dev not running — a running
// one changes files while they are read and holds them open while they are
// replaced.
func stoppedLocal(ctx context.Context, eng dockerEngine, env config.Environment, what string) error {
	if !env.Local() {
		return fmt.Errorf("%q is a remote environment — %s it on its own machine", env.Name, what)
	}
	if env.Archived != "" {
		return archivedError(env)
	}
	status, err := (ops.Runner{Engine: eng}).Inspect(ctx, env.ID)
	if err != nil {
		return err
	}
	if status.Dev != nil && status.Dev.Status != "exited" && status.Dev.Status != "created" && status.Dev.Status != "dead" {
		return fmt.Errorf("%q is running — stop it first (bh down %s)", env.Name, env.Name)
	}
	return nil
}

// snapshotTaken is what taking a snapshot came to.
type snapshotTaken struct {
	Path    string
	Size    int64 // of the file
	Summary snapshot.Summary
	Keys    []string
}

// takeSnapshot writes the environment's data to path, or into its snapshot
// directory when path is empty; only names the volumes to take (nil: all a
// snapshot holds). The caller has checked stoppedLocal.
func (a *app) takeSnapshot(ctx context.Context, eng dockerEngine, store config.Store, env config.Environment, kind, note, path string, only []string) (snapshotTaken, error) {
	var volumes []snapshot.Volume
	for _, key := range plan.SnapshotKeys {
		if only != nil && !slices.Contains(only, key) {
			continue
		}
		exists, err := eng.VolumeExists(ctx, plan.Volume(env.ID, key))
		if err != nil {
			return snapshotTaken{}, err
		}
		if exists {
			volumes = append(volumes, snapshot.Volume{Key: key, LeftOut: plan.LeftOut(key)})
		}
	}
	if len(volumes) == 0 {
		return snapshotTaken{}, fmt.Errorf("%q has no data yet: it was never started", env.Name)
	}
	image, err := a.devImage(ctx, eng, store, env.ID, nil)
	if err != nil {
		return snapshotTaken{}, err
	}
	info, err := eng.Info(ctx)
	if err != nil {
		return snapshotTaken{}, err
	}

	if path == "" {
		dir := store.SnapshotsDir(env.ID)
		// Snapshots hold what a home directory holds.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return snapshotTaken{}, err
		}
		// What an interrupted run left behind is never a snapshot.
		if stale, _ := filepath.Glob(filepath.Join(dir, "*.partial")); len(stale) > 0 {
			for _, name := range stale {
				os.Remove(name)
			}
		}
		path = filepath.Join(dir, snapshotName(a.now()))
		for n := 2; ; n++ {
			if _, err := os.Lstat(path); err != nil {
				break
			}
			path = filepath.Join(dir, strings.TrimSuffix(snapshotName(a.now()), snapshot.Extension)+fmt.Sprintf("-%d", n)+snapshot.Extension)
		}
	}

	writer, err := snapshot.Create(path, snapshot.Manifest{
		Kind: kind, Environment: snapshot.Environment{ID: env.ID, Name: env.Name},
		Created: a.now().UTC().Truncate(time.Second), Note: note,
		BH: version, Engine: info.OS + "/" + info.Arch, Volumes: volumes,
	})
	if err != nil {
		return snapshotTaken{}, err
	}
	dockerfile, err := os.ReadFile(store.DevDockerfile(env.ID))
	if err == nil {
		err = writer.AddDockerfile(dockerfile)
	} else if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	if err != nil {
		writer.Abort()
		return snapshotTaken{}, err
	}
	taken := snapshotTaken{Path: path}
	for _, volume := range volumes {
		fmt.Fprintf(a.stderr, "Reading %s...\n", volume.Key)
		job := engine.VolumeTar{Image: image, Volume: plan.Volume(env.ID, volume.Key), Target: plan.VolumeTarget(volume.Key), LeftOut: volume.LeftOut}
		count, err := writer.AddVolume(volume.Key, func(out io.Writer) error { return eng.ReadVolume(ctx, job, out) })
		if err != nil {
			writer.Abort()
			return snapshotTaken{}, fmt.Errorf("reading the %s volume: %w", volume.Key, err)
		}
		fmt.Fprintf(a.stderr, "  %s: %d entries, %s\n", volume.Key, count.Entries, tui.HumanBytes(count.Bytes))
		taken.Keys = append(taken.Keys, volume.Key)
	}
	if taken.Summary, err = writer.Close(); err != nil {
		return snapshotTaken{}, err
	}
	if stat, err := os.Stat(path); err == nil {
		taken.Size = stat.Size()
	}
	return taken, nil
}

// fillVolumes puts the snapshot's volumes (keys; nil for all it holds)
// into the environment's volumes, creating those that are missing. The
// snapshot has passed snapshot.Verify.
func (a *app) fillVolumes(ctx context.Context, eng dockerEngine, store config.Store, env config.Environment, path string, keys []string) error {
	owner, err := store.Owner()
	if err != nil {
		return err
	}
	image, err := a.devImage(ctx, eng, store, env.ID, nil)
	if err != nil {
		return err
	}
	_, err = snapshot.Restore(path, keys, func(volume snapshot.Volume, in io.Reader) error {
		if !slices.Contains(plan.SnapshotKeys, volume.Key) {
			return fmt.Errorf("this bh does not know a volume called %q", volume.Key)
		}
		name := plan.Volume(env.ID, volume.Key)
		if err := eng.EnsureVolume(ctx, name, plan.Labels(owner, env.ID, volume.Key)); err != nil {
			return err
		}
		fmt.Fprintf(a.stderr, "Writing %s...\n", volume.Key)
		return eng.ReplaceVolume(ctx, engine.VolumeTar{Image: image, Volume: name, Target: plan.VolumeTarget(volume.Key), LeftOut: volume.LeftOut}, in)
	})
	return err
}

func (a *app) snapshot(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("%s", snapshotUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		flags := flag.NewFlagSet("snapshot create", flag.ContinueOnError)
		note := flags.String("note", "", "")
		words, err := parseFlags(flags, rest)
		if err != nil {
			return err
		}
		if len(words) != 1 {
			return usagef("usage: bh snapshot create <env> [--note <text>]")
		}
		taken, env, err := a.createSnapshot(ctx, words[0], snapshot.KindManual, *note, "")
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Snapshot of %q: %s (%s)\n", env.Name, filepath.Base(taken.Path), tui.HumanBytes(taken.Size))
		return nil

	case "list":
		if len(rest) > 1 {
			return usagef("usage: bh snapshot list [<env>]")
		}
		ref := ""
		if len(rest) == 1 {
			ref = rest[0]
		}
		return a.printSnapshots(ref)

	case "remove":
		if len(rest) != 2 {
			return usagef("usage: bh snapshot remove <env> <snapshot>")
		}
		store, cfg, err := a.load()
		if err != nil {
			return err
		}
		env, err := cfg.Find(rest[0])
		if err != nil {
			return err
		}
		path, err := findSnapshot(store, env, rest[1])
		if err != nil {
			return err
		}
		if filepath.Dir(path) != store.SnapshotsDir(env.ID) {
			return fmt.Errorf("%s is not one of the snapshots of %q — it is a file of yours", path, env.Name)
		}
		if env.Archived == filepath.Base(path) {
			return fmt.Errorf("%s is what %q is archived in: removing it would lose the environment's data (bh unarchive %s first, or bh destroy %s)",
				filepath.Base(path), env.Name, env.Name, env.Name)
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Removed the snapshot %s of %q\n", filepath.Base(path), env.Name)
		return nil
	}
	return usagef("%s", snapshotUsage)
}

// createSnapshot takes a snapshot of the environment called ref, into its
// snapshot directory or to path.
func (a *app) createSnapshot(ctx context.Context, ref, kind, note, path string) (snapshotTaken, config.Environment, error) {
	store, cfg, err := a.load()
	if err != nil {
		return snapshotTaken{}, config.Environment{}, err
	}
	env, err := cfg.Find(ref)
	if err != nil {
		return snapshotTaken{}, env, err
	}
	eng, disconnect, err := a.dial(a.getenv)
	if err != nil {
		return snapshotTaken{}, env, err
	}
	defer disconnect()
	if err := stoppedLocal(ctx, eng, env, "take the snapshot of"); err != nil {
		return snapshotTaken{}, env, err
	}
	taken, err := a.takeSnapshot(ctx, eng, store, env, kind, note, path, nil)
	return taken, env, err
}

func (a *app) printSnapshots(ref string) error {
	store, cfg, err := a.load()
	if err != nil {
		return err
	}
	type group struct {
		title string
		id    string
	}
	var groups []group
	if ref != "" {
		env, err := cfg.Find(ref)
		if err != nil {
			return err
		}
		groups = []group{{env.Name, env.ID}}
	} else {
		known := map[string]bool{}
		for _, env := range cfg.Envs {
			groups = append(groups, group{env.Name, env.ID})
			known[env.ID] = true
		}
		// Snapshots outlive their environment: they are the way back from
		// a destroy.
		entries, _ := os.ReadDir(store.SnapshotsRoot())
		for _, entry := range entries {
			if entry.IsDir() && !known[entry.Name()] && config.ValidID(entry.Name()) {
				groups = append(groups, group{"", entry.Name()})
			}
		}
	}
	printed := 0
	for _, g := range groups {
		snapshots, err := listSnapshots(store, g.id)
		if err != nil {
			return err
		}
		if len(snapshots) == 0 {
			continue
		}
		title := g.title
		if title == "" {
			// Named by what the snapshots say: the environment is gone.
			title = "(destroyed)"
			for _, s := range snapshots {
				if s.Err == nil {
					title = s.Manifest.Environment.Name + " (destroyed — bh import " + s.Path + ")"
				}
			}
		}
		fmt.Fprintf(a.stdout, "%s\n", title)
		for _, s := range snapshots {
			if s.Err != nil {
				fmt.Fprintf(a.stdout, "  %s  unreadable: %v\n", s.Name, s.Err)
				continue
			}
			line := fmt.Sprintf("  %s  %8s  %-15s  %s", strings.TrimSuffix(s.Name, snapshot.Extension), tui.HumanBytes(s.Size), s.Manifest.Kind, strings.Join(s.Manifest.Keys(), "+"))
			if s.Manifest.Note != "" {
				line += "  " + s.Manifest.Note
			}
			fmt.Fprintln(a.stdout, line)
		}
		printed++
	}
	if printed == 0 {
		fmt.Fprintln(a.stdout, "No snapshots (bh snapshot create <env>)")
	}
	return nil
}

// export writes a snapshot of the environment to a file of the user's
// choosing: the copy to keep somewhere else.
func (a *app) export(ctx context.Context, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return usagef("usage: bh export <env> [<file or directory>]")
	}
	store, cfg, err := a.load()
	if err != nil {
		return err
	}
	env, err := cfg.Find(args[0])
	if err != nil {
		return err
	}
	if env.Archived != "" {
		return fmt.Errorf("%q is archived: its data is already one file — copy %s", env.Name, filepath.Join(store.SnapshotsDir(env.ID), env.Archived))
	}
	path := "."
	if len(args) == 2 {
		path = args[1]
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		path = filepath.Join(path, "bh-"+env.Name+"-"+snapshotName(a.now()))
	}
	taken, _, err := a.createSnapshot(ctx, env.Name, snapshot.KindExport, "", path)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Exported %q to %s (%s)\n", env.Name, taken.Path, tui.HumanBytes(taken.Size))
	fmt.Fprintln(a.stdout, "It holds the home directory as it is — logins, keys, history. bh import <file> makes an environment from it.")
	return nil
}

// rollbackPlan is what a rollback would do, for the question before it.
type rollbackPlan struct {
	Env      config.Environment
	Path     string
	Manifest snapshot.Manifest
	Keys     []string
}

// planRollback finds the snapshot and what of it goes back.
func (a *app) planRollback(ref, snapshotRef, only string) (config.Store, rollbackPlan, error) {
	store, cfg, err := a.load()
	if err != nil {
		return store, rollbackPlan{}, err
	}
	env, err := cfg.Find(ref)
	if err != nil {
		return store, rollbackPlan{}, err
	}
	if env.Archived != "" {
		return store, rollbackPlan{}, archivedError(env)
	}
	path, err := findSnapshot(store, env, snapshotRef)
	if err != nil {
		return store, rollbackPlan{}, err
	}
	manifest, err := snapshot.ReadManifest(path)
	if err != nil {
		return store, rollbackPlan{}, err
	}
	// Another environment's data over this one's is an import, not a
	// rollback: it must not happen by a mistyped name.
	if manifest.Environment.ID != env.ID {
		return store, rollbackPlan{}, fmt.Errorf("%s is a snapshot of %q, not of %q — bh import makes a new environment from it",
			path, manifest.Environment.Name, env.Name)
	}
	keys := manifest.Keys()
	if only != "" {
		if !slices.Contains(keys, only) {
			return store, rollbackPlan{}, fmt.Errorf("the snapshot holds %s, not %q", strings.Join(keys, " and "), only)
		}
		keys = []string{only}
	}
	return store, rollbackPlan{Env: env, Path: path, Manifest: manifest, Keys: keys}, nil
}

// rollBack replaces the environment's data with the snapshot's. before:
// take a snapshot of the data as it is now first, so that there is a way
// back from this too.
func (a *app) rollBack(ctx context.Context, store config.Store, planned rollbackPlan, before bool) (kept string, err error) {
	env := planned.Env
	eng, disconnect, err := a.dial(a.getenv)
	if err != nil {
		return "", err
	}
	defer disconnect()
	if err := stoppedLocal(ctx, eng, env, "roll back"); err != nil {
		return "", err
	}
	// Read to its end before anything is emptied for it.
	fmt.Fprintln(a.stderr, "Checking the snapshot...")
	if _, err := snapshot.Verify(planned.Path); err != nil {
		return "", fmt.Errorf("the snapshot is damaged, nothing was changed: %w", err)
	}
	if before {
		taken, err := a.takeSnapshot(ctx, eng, store, env, snapshot.KindBeforeRollback, "before rolling back to "+strings.TrimSuffix(filepath.Base(planned.Path), snapshot.Extension), "", nil)
		if err != nil {
			return "", fmt.Errorf("could not keep the current data first, nothing was changed: %w", err)
		}
		kept = filepath.Base(taken.Path)
	}
	if err := a.fillVolumes(ctx, eng, store, env, planned.Path, planned.Keys); err != nil {
		hint := "run the rollback again"
		if kept != "" {
			hint += fmt.Sprintf(", or go back to how it was with bh rollback %s %s", env.Name, strings.TrimSuffix(kept, snapshot.Extension))
		}
		return kept, fmt.Errorf("%w\n(the volumes may be half replaced — %s)", err, hint)
	}
	return kept, nil
}

func (a *app) rollback(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("rollback", flag.ContinueOnError)
	only := flags.String("only", "", "")
	noSnapshot := flags.Bool("no-snapshot", false, "")
	yes := flags.Bool("yes", false, "")
	words, err := parseFlags(flags, args)
	if err != nil {
		return err
	}
	if len(words) != 2 {
		return usagef("usage: bh rollback <env> <snapshot | latest | file> [--only workspace|home] [--no-snapshot] [--yes]")
	}
	store, planned, err := a.planRollback(words[0], words[1], *only)
	if err != nil {
		return err
	}
	name := planned.Env.Name
	if !*yes {
		fmt.Fprintf(a.stderr, "This replaces the %s of %q with the snapshot of %s.\n",
			strings.Join(planned.Keys, " and "), name, planned.Manifest.Created.Local().Format("2006-01-02 15:04"))
		if *noSnapshot {
			fmt.Fprintln(a.stderr, "What is there now is deleted.")
		} else {
			fmt.Fprintln(a.stderr, "What is there now is kept in a snapshot first.")
		}
		if !a.confirm("Roll back?") {
			return errAborted
		}
	}
	kept, err := a.rollBack(ctx, store, planned, !*noSnapshot)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Rolled %q back to %s (%s)\n", name, strings.TrimSuffix(filepath.Base(planned.Path), snapshot.Extension), strings.Join(planned.Keys, ", "))
	if kept != "" {
		fmt.Fprintf(a.stdout, "How it was is in the snapshot %s\n", strings.TrimSuffix(kept, snapshot.Extension))
	}
	fmt.Fprintf(a.stdout, "Start it with bh up %s. If tools are missing, bh init %s runs init.sh again.\n", name, name)
	return nil
}

// importSnapshot makes a new environment from a snapshot file: default
// settings (they are this machine's to decide), the file's dev.Dockerfile,
// and its data in fresh volumes.
func (a *app) importSnapshot(ctx context.Context, path, name string) (config.Environment, error) {
	fmt.Fprintln(a.stderr, "Checking the snapshot...")
	contents, err := snapshot.Verify(path)
	if err != nil {
		return config.Environment{}, err
	}
	if name == "" {
		name = contents.Manifest.Environment.Name
	}
	store, env, err := a.newLocal(name, "", nil)
	if err != nil {
		return env, err
	}
	// From here on there is an environment: what goes wrong is reported
	// with its name, and it is the user's to keep or to destroy.
	failed := func(err error) (config.Environment, error) {
		return env, fmt.Errorf("%w\n(%q exists but is incomplete — bh destroy %s removes it)", err, env.Name, env.Name)
	}
	if contents.Dockerfile != nil {
		if err := os.WriteFile(store.DevDockerfile(env.ID), contents.Dockerfile, 0o644); err != nil {
			return failed(err)
		}
	}
	eng, disconnect, err := a.dial(a.getenv)
	if err != nil {
		return failed(err)
	}
	defer disconnect()
	if err := a.fillVolumes(ctx, eng, store, env, path, nil); err != nil {
		return failed(err)
	}
	a.syncSSH(store)
	return env, nil
}

func (a *app) importCommand(ctx context.Context, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return usagef("usage: bh import <file> [<name>]")
	}
	name := ""
	if len(args) == 2 {
		name = args[1]
	}
	env, err := a.importSnapshot(ctx, args[0], name)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Imported %q (ssh port %d). Start it with bh up %s — init.sh runs and installs the tools.\n", env.Name, env.SSHPort, env.Name)
	fmt.Fprintln(a.stdout, "Settings, keys and tokens are not part of a snapshot: bh configure, bh key attach, bh github set.")
	return nil
}

// archived is what putting an environment away came to.
type archived struct {
	Env   config.Environment
	Taken snapshotTaken
	// Revoked are the services whose tokens it had, which are revoked now.
	Revoked []string
	// Warnings are what is left to do by hand.
	Warnings []string
}

// archiveEnv puts the environment away: a snapshot, read back whole, and
// only then the containers and volumes go — and after them its GitHub
// token, which an environment that is asleep has no use for.
func (a *app) archiveEnv(ctx context.Context, ref string) (archived, error) {
	store, cfg, err := a.load()
	if err != nil {
		return archived{}, err
	}
	env, err := cfg.Find(ref)
	if err != nil {
		return archived{}, err
	}
	result := archived{Env: env}
	eng, disconnect, err := a.dial(a.getenv)
	if err != nil {
		return result, err
	}
	defer disconnect()
	if err := stoppedLocal(ctx, eng, env, "archive"); err != nil {
		return result, err
	}
	taken, err := a.takeSnapshot(ctx, eng, store, env, snapshot.KindArchive, "", "", nil)
	if err != nil {
		return result, err
	}
	result.Taken = taken
	// The file is all there will be of the data: read it back before the
	// volumes go.
	fmt.Fprintln(a.stderr, "Checking the snapshot...")
	if _, err := snapshot.Verify(taken.Path); err != nil {
		return result, fmt.Errorf("the snapshot did not read back, nothing was removed: %w", err)
	}
	env.Archived = filepath.Base(taken.Path)
	if err := store.SaveEnvironment(env); err != nil {
		return result, err
	}
	result.Env = env
	if err := (ops.Runner{Engine: eng}).Archive(ctx, env.ID); err != nil {
		return result, fmt.Errorf("the snapshot is taken and %q is marked archived, but removing its containers and volumes failed: %w\n(bh unarchive %s puts it back as it was)", env.Name, err, env.Name)
	}
	// After Docker's part, as with destroy: had that failed, the
	// environment would still be there to use, and its tokens with it. An
	// environment that is asleep should not hold a credential that works.
	// A token that cannot be revoked keeps its record: it is what a retry
	// needs. Secrets and variables stay; they come back with bh unarchive.
	if creds, err := a.credentialsOf(store, env, false); err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("the token records could not be read, so no token was revoked: %v", err))
	} else {
		result.Revoked, result.Warnings = a.revokeTokens(ctx, creds, true)
	}
	a.syncSSH(store)
	return result, nil
}

func (a *app) archive(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("archive", flag.ContinueOnError)
	yes := flags.Bool("yes", false, "")
	words, err := parseFlags(flags, args)
	if err != nil {
		return err
	}
	if len(words) != 1 {
		return usagef("usage: bh archive <env> [--yes]")
	}
	if !*yes {
		fmt.Fprintf(a.stderr, "This takes a snapshot of %q and then removes its containers and volumes.\n", words[0])
		fmt.Fprintln(a.stderr, "The workspace and the home directory come back with bh unarchive; the nix store")
		fmt.Fprintln(a.stderr, "and caches do not — init.sh installs the tools again. The tokens of the")
		fmt.Fprintln(a.stderr, "environment (GitHub, Hugging Face) are revoked.")
		if !a.confirm("Archive?") {
			return errAborted
		}
	}
	result, err := a.archiveEnv(ctx, words[0])
	if err != nil {
		return err
	}
	name := result.Env.Name
	fmt.Fprintf(a.stdout, "Archived %q in %s (%s). bh unarchive %s brings it back.\n", name, result.Taken.Path, tui.HumanBytes(result.Taken.Size), name)
	if len(result.Revoked) > 0 {
		fmt.Fprintf(a.stdout, "Revoked its tokens: %s (bh token set %s after bh unarchive gets new ones).\n", strings.Join(result.Revoked, ", "), name)
	}
	a.warn(result.Warnings)
	return nil
}

// unarchiveEnv brings an archived environment's data back into volumes.
// It can be run again after a failure: the mark stays until all is back.
func (a *app) unarchiveEnv(ctx context.Context, ref string) (config.Environment, error) {
	store, cfg, err := a.load()
	if err != nil {
		return config.Environment{}, err
	}
	env, err := cfg.Find(ref)
	if err != nil {
		return env, err
	}
	if env.Archived == "" {
		return env, fmt.Errorf("%q is not archived", env.Name)
	}
	path := filepath.Join(store.SnapshotsDir(env.ID), env.Archived)
	fmt.Fprintln(a.stderr, "Checking the snapshot...")
	if _, err := snapshot.Verify(path); err != nil {
		return env, fmt.Errorf("the snapshot %q is archived in cannot be used: %w", env.Name, err)
	}
	eng, disconnect, err := a.dial(a.getenv)
	if err != nil {
		return env, err
	}
	defer disconnect()
	// An archive that stopped half way may have left containers.
	if err := (ops.Runner{Engine: eng}).Down(ctx, env.ID); err != nil {
		return env, err
	}
	if err := a.fillVolumes(ctx, eng, store, env, path, nil); err != nil {
		return env, fmt.Errorf("%w\n(%q is still archived and its snapshot is untouched — run bh unarchive %s again)", err, env.Name, env.Name)
	}
	env.Archived = ""
	if err := store.SaveEnvironment(env); err != nil {
		return env, err
	}
	a.syncSSH(store)
	return env, nil
}

func (a *app) unarchive(ctx context.Context, args []string) error {
	ref, err := oneEnv("unarchive", args)
	if err != nil {
		return err
	}
	env, err := a.unarchiveEnv(ctx, ref)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%q is back. Start it with bh up %s — init.sh runs and installs the tools.\n", env.Name, env.Name)
	fmt.Fprintln(a.stdout, "The snapshot it was archived in stays (bh snapshot list).")
	return nil
}
