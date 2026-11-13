package main

import (
	"errors"
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ys319/bulkhead/internal/plan"
	"github.com/ys319/bulkhead/internal/snapshot"
)

// imageHome is what the dev image has in the home directory of what a
// reset of the tools takes back.
var imageHome = map[string]map[string]string{
	"/home/developer": {
		".nix-profile":                      "-> image profile",
		".local/state/nix/profiles/profile": "-> /nix/store/image",
		".zshrc":                            "the image's zshrc",
	},
}

func TestResetAsksAndNeedsYesAwayFromATerminal(t *testing.T) {
	b := newBench(t)
	id := withData(t, b, "web")
	b.ok("up", "web")
	untouched := func() {
		t.Helper()
		for _, key := range plan.AllVolumeKeys() {
			if !b.engine.Volumes[plan.Volume(id, key)] {
				t.Fatalf("the %s volume is gone although reset was not confirmed", key)
			}
		}
		if b.volume(id, "home")[".cache/big"] != "cache" {
			t.Fatal("the home directory changed although reset was not confirmed")
		}
	}
	for _, args := range [][]string{{"reset", "web"}, {"reset", "web", "--home"}} {
		b.typing = false
		if err := b.fails(1, args...); !strings.Contains(err, "--yes") {
			t.Errorf("bh %s away from a terminal: %s", strings.Join(args, " "), err)
		}
		b.typing, b.stdin = true, "n\n"
		if err := b.fails(1, args...); !strings.Contains(err, "not confirmed") {
			t.Errorf("bh %s, answered no: %s", strings.Join(args, " "), err)
		}
		untouched()
	}
	if err := b.fails(2, "reset", "web", "--no-snapshot"); !strings.Contains(err, "usage: bh reset") {
		t.Errorf("--no-snapshot without --home: %s", err)
	}
}

func TestResetOfTheToolsKeepsWhatIsTheUsers(t *testing.T) {
	b := newBench(t)
	id := withData(t, b, "web")
	b.engine.ImageFiles = imageHome
	b.ok("up", "web")

	out := b.ok("reset", "web", "--yes")
	if !strings.Contains(out, `Reset the tools of "web"`) || !strings.Contains(out, "runs init.sh again") {
		t.Errorf("output:\n%s", out)
	}
	for key, want := range map[string]bool{"nix": false, "home": true, "workspace": true, "hostkeys": true, "github": true, "tokens": true} {
		if got := b.engine.Volumes[plan.Volume(id, key)]; got != want {
			t.Errorf("the %s volume exists = %v, want %v", key, got, want)
		}
	}
	if len(b.engine.Containers) != 0 {
		t.Errorf("containers stayed: %v", b.engine.Containers)
	}
	// In the home directory: the user's files as they were; what points
	// into the nix store as the image has it; caches and the mark of
	// init.sh gone.
	want := map[string]string{
		".zshrc": "export A=1", ".claude/state.json": "{}",
		".nix-profile": "-> image profile", ".local/state/nix/profiles/profile": "-> /nix/store/image",
	}
	if got := b.volume(id, "home"); !maps.Equal(got, want) {
		t.Errorf("home = %v", got)
	}
	if got := b.volume(id, "workspace"); got["notes.txt"] != "v1" || len(got) != 2 {
		t.Errorf("workspace = %v", got)
	}
	// Nothing of the user's went, so nothing was kept.
	if left := b.snapshots(id); len(left) != 0 {
		t.Errorf("snapshots: %v", left)
	}
	b.ok("up", "web")
}

func TestResetOfTheToolsStopsBeforeTheStoreGoes(t *testing.T) {
	b := newBench(t)
	id := withData(t, b, "web")
	b.engine.VolumeTarErr = errors.New("the engine went away")
	if err := b.fails(1, "reset", "web", "--yes"); !strings.Contains(err, "nothing else was changed") {
		t.Errorf("error: %s", err)
	}
	if !b.engine.Volumes[plan.Volume(id, "nix")] {
		t.Error("the nix store went although the home directory was not prepared for a new one")
	}
}

func TestResetOfTheHomeDirectoryKeepsItInASnapshot(t *testing.T) {
	b := newBench(t)
	id := withData(t, b, "web")
	b.ok("up", "web")

	out := b.ok("reset", "web", "--home", "--yes")
	if !strings.Contains(out, `Reset "web" (home and nix are deleted; the workspace is kept)`) ||
		!strings.Contains(out, "is in the snapshot 2026-11-13T090000Z (bh rollback web 2026-11-13T090000Z)") {
		t.Errorf("output:\n%s", out)
	}
	for key, want := range map[string]bool{"home": false, "nix": false, "workspace": true, "hostkeys": true, "github": true, "tokens": true} {
		if got := b.engine.Volumes[plan.Volume(id, key)]; got != want {
			t.Errorf("the %s volume exists = %v, want %v", key, got, want)
		}
	}
	// The snapshot is of the home directory alone: the workspace was not
	// at risk, and reading it would only take time.
	names := b.snapshots(id)
	if len(names) != 1 {
		t.Fatalf("snapshots = %v", names)
	}
	contents, err := snapshot.Verify(filepath.Join(b.dir, "snapshots", id, names[0]))
	if err != nil {
		t.Fatal(err)
	}
	if m := contents.Manifest; m.Kind != snapshot.KindBeforeReset || strings.Join(m.Keys(), ",") != "home" {
		t.Errorf("manifest = %+v", m)
	}

	// And it is the way back.
	b.ok("up", "web")
	b.ok("down", "web")
	b.tick()
	b.ok("rollback", "web", "latest", "--yes", "--no-snapshot")
	if got := b.volume(id, "home"); got[".zshrc"] != "export A=1" || got[".claude/state.json"] != "{}" {
		t.Errorf("home after the rollback = %v", got)
	}
}

func TestResetOfTheHomeDirectoryWithoutASnapshot(t *testing.T) {
	b := newBench(t)
	id := withData(t, b, "web")

	// It cannot be kept: nothing goes.
	b.engine.VolumeTarErr = errors.New("no space left")
	if err := b.fails(1, "reset", "web", "--home", "--yes"); !strings.Contains(err, "could not keep the home directory first, nothing was removed") {
		t.Errorf("error: %s", err)
	}
	if !b.engine.Volumes[plan.Volume(id, "home")] {
		t.Fatal("the home volume went without a snapshot")
	}

	// Declined: it goes at once, and reads nothing.
	b.engine.VolumeTars = nil
	b.ok("reset", "web", "--home", "--no-snapshot", "--yes")
	if b.engine.Volumes[plan.Volume(id, "home")] || len(b.snapshots(id)) != 0 || len(b.engine.VolumeTars) != 0 {
		t.Errorf("home exists = %v, snapshots = %v, read = %v", b.engine.Volumes[plan.Volume(id, "home")], b.snapshots(id), b.engine.VolumeTars)
	}
	if !b.engine.Volumes[plan.Volume(id, "workspace")] {
		t.Error("the workspace went")
	}
}
