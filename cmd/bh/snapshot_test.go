package main

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ys319/bulkhead/internal/snapshot"
)

// withData starts an environment once, stops it, and puts files into its
// volumes — the way a used environment looks to a snapshot.
func withData(t *testing.T, b *bench, name string) (id string) {
	t.Helper()
	b.now = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	b.ok("create", name)
	b.ok("up", name)
	b.ok("down", name)
	id = b.env(name).ID
	b.engine.Data = map[string]map[string]string{
		"bulkhead-" + id + "_workspace": {"src/main.go": "package main", "notes.txt": "v1"},
		"bulkhead-" + id + "_home": {
			".zshrc": "export A=1", ".claude/state.json": "{}",
			// What a snapshot leaves out.
			".cache/big": "cache", ".local/state/nix/profiles/profile": "-> /nix/store/x",
			".config/bulkhead/initialized": "", ".vscode-server/bin": "server",
		},
		"bulkhead-" + id + "_nix": {"store/x": "tool"},
	}
	return id
}

// tick moves the clock, so that the next snapshot gets a later name.
func (b *bench) tick() { b.now = b.now.Add(time.Minute) }

func (b *bench) snapshots(id string) []string {
	b.t.Helper()
	entries, err := os.ReadDir(filepath.Join(b.dir, "snapshots", id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		b.t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func (b *bench) volume(id, key string) map[string]string {
	return b.engine.Data["bulkhead-"+id+"_"+key]
}

func TestSnapshotHoldsWorkspaceAndHomeWithoutWhatComesBack(t *testing.T) {
	b := newBench(t)
	id := withData(t, b, "web")

	out := b.ok("snapshot", "create", "web", "--note", "before the upgrade")
	if !strings.Contains(out, `Snapshot of "web": 2026-11-13T090000Z.tar.gz`) {
		t.Errorf("output: %s", out)
	}
	names := b.snapshots(id)
	if !slices.Equal(names, []string{"2026-11-13T090000Z.tar.gz"}) {
		t.Fatalf("snapshots = %v", names)
	}
	path := filepath.Join(b.dir, "snapshots", id, names[0])
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the file: %v %v", info, err)
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the directory: %v %v", info, err)
	}
	contents, err := snapshot.Verify(path)
	if err != nil {
		t.Fatal(err)
	}
	m := contents.Manifest
	if m.Kind != snapshot.KindManual || m.Environment.ID != id || m.Environment.Name != "web" || m.Note != "before the upgrade" ||
		!slices.Equal(m.Keys(), []string{"workspace", "home"}) {
		t.Errorf("manifest = %+v", m)
	}
	// The root and two files; the root and two files, the left-out four not.
	if got := contents.Summary.Volumes; got["workspace"].Entries != 3 || got["home"].Entries != 3 {
		t.Errorf("summary = %+v", got)
	}
	// Reading does not go near the nix volume or the ssh identity.
	for _, job := range b.engine.VolumeTars {
		if strings.HasSuffix(job.Volume, "_nix") || strings.HasSuffix(job.Volume, "_hostkeys") {
			t.Errorf("read %s", job.Volume)
		}
	}

	list := b.ok("snapshot", "list")
	if !strings.Contains(list, "web\n") || !strings.Contains(list, "2026-11-13T090000Z") || !strings.Contains(list, "manual") ||
		!strings.Contains(list, "workspace+home") || !strings.Contains(list, "before the upgrade") {
		t.Errorf("list:\n%s", list)
	}

	b.ok("snapshot", "remove", "web", "2026-11-13T090000Z")
	if left := b.snapshots(id); len(left) != 0 {
		t.Errorf("after remove: %v", left)
	}
}

func TestSnapshotNeedsAStoppedEnvironmentWithData(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	if err := b.fails(1, "snapshot", "create", "web"); !strings.Contains(err, "no data yet") {
		t.Errorf("never started: %s", err)
	}
	b.ok("up", "web")
	if err := b.fails(1, "snapshot", "create", "web"); !strings.Contains(err, "is running") || !strings.Contains(err, "bh down web") {
		t.Errorf("running: %s", err)
	}
	if len(b.engine.VolumeTars) != 0 {
		t.Errorf("volumes were read: %v", b.engine.VolumeTars)
	}
	b.ok("remote", "add", "studio.test:22001/far")
	if err := b.fails(1, "snapshot", "create", "far"); !strings.Contains(err, "remote environment") {
		t.Errorf("remote: %s", err)
	}
}

func TestAFailedReadLeavesNoSnapshot(t *testing.T) {
	b := newBench(t)
	id := withData(t, b, "web")
	b.engine.VolumeTarErr = errors.New("the container died")
	if err := b.fails(1, "snapshot", "create", "web"); !strings.Contains(err, "the container died") {
		t.Errorf("error: %s", err)
	}
	if left := b.snapshots(id); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestRollbackPutsTheDataBackAndKeepsWhatWasThere(t *testing.T) {
	b := newBench(t)
	id := withData(t, b, "web")
	b.ok("snapshot", "create", "web")

	// Work goes on; nix changes too.
	b.tick()
	b.volume(id, "workspace")["notes.txt"] = "v2"
	b.volume(id, "workspace")["new.txt"] = "new"
	b.volume(id, "home")[".zshrc"] = "export A=2"
	b.volume(id, "home")[".cache/big"] = "newer cache"
	b.volume(id, "home")[".local/state/nix/profiles/profile"] = "-> /nix/store/y"
	b.volume(id, "nix")["store/y"] = "newer tool"

	if err := b.fails(1, "rollback", "web", "latest"); !strings.Contains(err, "not confirmed") {
		t.Errorf("unconfirmed: %s", err)
	}
	if b.volume(id, "workspace")["notes.txt"] != "v2" {
		t.Fatal("an unconfirmed rollback changed the workspace")
	}

	out := b.ok("rollback", "web", "latest", "--yes")
	if !strings.Contains(out, `Rolled "web" back to 2026-11-13T090000Z (workspace, home)`) || !strings.Contains(out, "How it was is in the snapshot 2026-11-13T090100Z") {
		t.Errorf("output: %s", out)
	}
	if got, want := b.volume(id, "workspace"), map[string]string{"src/main.go": "package main", "notes.txt": "v1"}; !maps.Equal(got, want) {
		t.Errorf("workspace = %v", got)
	}
	home := b.volume(id, "home")
	if home[".zshrc"] != "export A=1" {
		t.Errorf("home was not put back: %v", home)
	}
	// What the snapshot left out is left alone: it belongs to the nix
	// volume as it is now, which the rollback did not touch.
	if home[".cache/big"] != "newer cache" || home[".local/state/nix/profiles/profile"] != "-> /nix/store/y" || b.volume(id, "nix")["store/y"] != "newer tool" {
		t.Errorf("the left-out paths or nix changed: %v / %v", home, b.volume(id, "nix"))
	}
	if _, has := home[".config/bulkhead/initialized"]; !has {
		t.Error("the mark of init.sh is gone: it would run again against the nix store that is there")
	}

	// The state before the rollback is a snapshot too, and a way back.
	names := b.snapshots(id)
	if len(names) != 2 {
		t.Fatalf("snapshots = %v", names)
	}
	before, err := snapshot.ReadManifest(filepath.Join(b.dir, "snapshots", id, names[1]))
	if err != nil || before.Kind != snapshot.KindBeforeRollback {
		t.Errorf("the second snapshot: %+v %v", before, err)
	}
	b.tick()
	b.ok("rollback", "web", "2026-11-13T090100Z", "--yes", "--no-snapshot")
	if b.volume(id, "workspace")["notes.txt"] != "v2" || b.volume(id, "workspace")["new.txt"] != "new" {
		t.Errorf("back again: %v", b.volume(id, "workspace"))
	}
	if left := b.snapshots(id); len(left) != 2 {
		t.Errorf("--no-snapshot took one: %v", left)
	}
}

func TestRollbackOfOneVolume(t *testing.T) {
	b := newBench(t)
	id := withData(t, b, "web")
	b.ok("snapshot", "create", "web")
	b.tick()
	b.volume(id, "workspace")["notes.txt"] = "v2"
	b.volume(id, "home")[".zshrc"] = "export A=2"

	b.ok("rollback", "web", "latest", "--only", "home", "--yes", "--no-snapshot")
	if b.volume(id, "home")[".zshrc"] != "export A=1" || b.volume(id, "workspace")["notes.txt"] != "v2" {
		t.Errorf("home = %v, workspace = %v", b.volume(id, "home"), b.volume(id, "workspace"))
	}
	if err := b.fails(1, "rollback", "web", "latest", "--only", "nix", "--yes"); !strings.Contains(err, `not "nix"`) {
		t.Errorf("--only nix: %s", err)
	}
}

func TestRollbackRefusesWhatWouldLoseData(t *testing.T) {
	b := newBench(t)
	id := withData(t, b, "web")
	b.ok("snapshot", "create", "web")
	path := filepath.Join(b.dir, "snapshots", id, "2026-11-13T090000Z.tar.gz")
	untouched := func(why string) {
		t.Helper()
		if b.volume(id, "workspace")["notes.txt"] != "v1" || len(b.volume(id, "workspace")) != 2 {
			t.Errorf("%s: the workspace changed: %v", why, b.volume(id, "workspace"))
		}
	}

	// Running.
	b.ok("up", "web")
	if err := b.fails(1, "rollback", "web", "latest", "--yes"); !strings.Contains(err, "is running") {
		t.Errorf("running: %s", err)
	}
	b.ok("down", "web")
	untouched("running")

	// Another environment's snapshot.
	b.ok("create", "api")
	b.ok("up", "api")
	b.ok("down", "api")
	if err := b.fails(1, "rollback", "api", path, "--yes"); !strings.Contains(err, `a snapshot of "web", not of "api"`) || !strings.Contains(err, "bh import") {
		t.Errorf("another environment's: %s", err)
	}

	// A damaged file: nothing is emptied for it, and no snapshot is taken.
	whole, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, whole[:len(whole)-9], 0o600); err != nil {
		t.Fatal(err)
	}
	b.engine.VolumeTars = nil
	if err := b.fails(1, "rollback", "web", "latest", "--yes"); !strings.Contains(err, "damaged, nothing was changed") {
		t.Errorf("damaged: %s", err)
	}
	untouched("damaged")
	if len(b.engine.VolumeTars) != 0 {
		t.Errorf("volumes were touched: %v", b.engine.VolumeTars)
	}
	if err := os.WriteFile(path, whole, 0o600); err != nil {
		t.Fatal(err)
	}

	// The data as it is cannot be kept first: nothing is replaced.
	b.tick()
	b.engine.VolumeTarErr = errors.New("no space left")
	if err := b.fails(1, "rollback", "web", "latest", "--yes"); !strings.Contains(err, "could not keep the current data first, nothing was changed") {
		t.Errorf("no way back: %s", err)
	}
	b.engine.VolumeTarErr = nil
	untouched("no way back")
}

func TestExportAndImport(t *testing.T) {
	b := newBench(t)
	id := withData(t, b, "web")
	if err := os.WriteFile(filepath.Join(b.dir, "environments", id, "dev.Dockerfile"), []byte("FROM bulkhead-dev-base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "kept")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	b.ok("export", "web", out)
	file := filepath.Join(out, "bh-web-2026-11-13T090000Z.tar.gz")
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}
	// An export is not one of the environment's snapshots.
	if left := b.snapshots(id); len(left) != 0 {
		t.Errorf("export left %v in the snapshot directory", left)
	}

	// The name is taken: nothing is made.
	if err := b.fails(1, "import", file); !strings.Contains(err, "web") {
		t.Errorf("same name: %s", err)
	}
	b.ok("import", file, "web2")
	imported := b.env("web2")
	original := b.env("web")
	if imported.ID == original.ID || imported.SSHPort == original.SSHPort || imported.Archived != "" {
		t.Errorf("imported = %+v, original = %+v", imported, original)
	}
	dockerfile, err := os.ReadFile(filepath.Join(b.dir, "environments", imported.ID, "dev.Dockerfile"))
	if err != nil || string(dockerfile) != "FROM bulkhead-dev-base\n" {
		t.Errorf("dev.Dockerfile: %q %v", dockerfile, err)
	}
	if got, want := b.volume(imported.ID, "workspace"), map[string]string{"src/main.go": "package main", "notes.txt": "v1"}; !maps.Equal(got, want) {
		t.Errorf("workspace = %v", got)
	}
	if got, want := b.volume(imported.ID, "home"), map[string]string{".zshrc": "export A=1", ".claude/state.json": "{}"}; !maps.Equal(got, want) {
		t.Errorf("home = %v", got)
	}
	// The original is as it was, and the new one starts like any other.
	if b.volume(id, "home")[".cache/big"] != "cache" {
		t.Error("the original's home changed")
	}
	b.ok("up", "web2")

	if err := b.fails(1, "import", filepath.Join(out, "missing.tar.gz")); err == "" {
		t.Error("a missing file was imported")
	}
}

func TestArchiveAndUnarchive(t *testing.T) {
	b := newBench(t)
	id := withData(t, b, "web")

	if err := b.fails(1, "archive", "web"); !strings.Contains(err, "not confirmed") {
		t.Errorf("unconfirmed: %s", err)
	}
	if b.env("web").Archived != "" || len(b.snapshots(id)) != 0 {
		t.Fatal("an unconfirmed archive did something")
	}

	b.ok("archive", "web", "--yes")
	if got := b.env("web").Archived; got != "2026-11-13T090000Z.tar.gz" {
		t.Errorf("archived = %q", got)
	}
	for _, key := range []string{"workspace", "home", "nix", "github", "tokens"} {
		if b.engine.Volumes["bulkhead-"+id+"_"+key] {
			t.Errorf("the %s volume is still there", key)
		}
	}
	if !b.engine.Volumes["bulkhead-"+id+"_hostkeys"] {
		t.Error("the ssh host identity is gone")
	}
	if len(b.engine.Containers) != 0 {
		t.Errorf("containers: %v", b.engine.Containers)
	}

	// Asleep: listed, and nothing starts it by accident.
	if list := b.ok("list"); !strings.Contains(list, "archived") {
		t.Errorf("list:\n%s", list)
	}
	if status := b.ok("status", "web"); !strings.Contains(status, "archived") || !strings.Contains(status, "2026-11-13T090000Z.tar.gz") {
		t.Errorf("status:\n%s", status)
	}
	for _, command := range [][]string{{"up", "web"}, {"restart", "web"}, {"ssh", "web"}, {"reset", "web", "--yes"}, {"snapshot", "create", "web"},
		{"rollback", "web", "latest", "--yes"}, {"archive", "web", "--yes"}, {"export", "web"}} {
		if err := b.fails(1, command...); !strings.Contains(err, "archived") {
			t.Errorf("bh %s: %s", strings.Join(command, " "), err)
		}
	}
	if len(b.engine.Containers) != 0 {
		t.Error("something was created for an archived environment")
	}
	// The file is the environment's data now.
	if err := b.fails(1, "snapshot", "remove", "web", "latest"); !strings.Contains(err, "would lose the environment's data") {
		t.Errorf("remove: %s", err)
	}

	b.ok("unarchive", "web")
	if b.env("web").Archived != "" {
		t.Error("still marked archived")
	}
	if got, want := b.volume(id, "workspace"), map[string]string{"src/main.go": "package main", "notes.txt": "v1"}; !maps.Equal(got, want) {
		t.Errorf("workspace = %v", got)
	}
	// Home without what a fresh nix volume and init.sh bring.
	if got, want := b.volume(id, "home"), map[string]string{".zshrc": "export A=1", ".claude/state.json": "{}"}; !maps.Equal(got, want) {
		t.Errorf("home = %v", got)
	}
	if len(b.snapshots(id)) != 1 {
		t.Errorf("the snapshot went with the unarchive: %v", b.snapshots(id))
	}
	b.ok("up", "web")
	if err := b.fails(1, "unarchive", "web"); !strings.Contains(err, "not archived") {
		t.Errorf("twice: %s", err)
	}
}

func TestArchiveRemovesNothingWithoutAGoodSnapshot(t *testing.T) {
	b := newBench(t)
	id := withData(t, b, "web")
	b.engine.VolumeTarErr = errors.New("no space left")
	if err := b.fails(1, "archive", "web", "--yes"); !strings.Contains(err, "no space left") {
		t.Errorf("error: %s", err)
	}
	if b.env("web").Archived != "" {
		t.Error("marked archived without a snapshot")
	}
	for _, key := range []string{"workspace", "home", "nix"} {
		if !b.engine.Volumes["bulkhead-"+id+"_"+key] {
			t.Errorf("the %s volume was removed", key)
		}
	}
}

func TestUnarchiveWithoutItsSnapshotChangesNothing(t *testing.T) {
	b := newBench(t)
	id := withData(t, b, "web")
	b.ok("archive", "web", "--yes")
	path := filepath.Join(b.dir, "snapshots", id, b.env("web").Archived)
	whole, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, whole[:len(whole)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.fails(1, "unarchive", "web"); !strings.Contains(err, "cannot be used") {
		t.Errorf("error: %s", err)
	}
	if b.env("web").Archived == "" {
		t.Error("the mark is gone though nothing came back")
	}
	if b.engine.Volumes["bulkhead-"+id+"_workspace"] {
		t.Error("a volume was made from a damaged snapshot")
	}

	// A failure while writing: still archived, and running it again works.
	if err := os.WriteFile(path, whole, 0o600); err != nil {
		t.Fatal(err)
	}
	b.engine.VolumeTarErr = errors.New("the engine went away")
	if err := b.fails(1, "unarchive", "web"); !strings.Contains(err, "still archived") {
		t.Errorf("error: %s", err)
	}
	b.engine.VolumeTarErr = nil
	b.ok("unarchive", "web")
	if b.volume(id, "workspace")["notes.txt"] != "v1" {
		t.Errorf("workspace = %v", b.volume(id, "workspace"))
	}
}

func TestSnapshotsOutliveADestroy(t *testing.T) {
	b := newBench(t)
	id := withData(t, b, "web")
	b.ok("snapshot", "create", "web")
	b.ok("destroy", "web", "--yes")
	if left := b.snapshots(id); len(left) != 1 {
		t.Fatalf("snapshots after destroy: %v", left)
	}
	list := b.ok("snapshot", "list")
	if !strings.Contains(list, "web (destroyed") || !strings.Contains(list, "bh import") {
		t.Errorf("list:\n%s", list)
	}
	b.ok("import", filepath.Join(b.dir, "snapshots", id, "2026-11-13T090000Z.tar.gz"))
	if b.volume(b.env("web").ID, "workspace")["notes.txt"] != "v1" {
		t.Error("the data did not come back")
	}
}
