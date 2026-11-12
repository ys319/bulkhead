package main

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/sshkey"
)

// legacyEnv puts an environment of the TypeScript version on disk, the way
// that version leaves it, and returns its directory.
func (b *bench) legacyEnv(name, envJSON string) string {
	b.t.Helper()
	dir := filepath.Join(b.legacy, "environments", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.t.Fatal(err)
	}
	for file, content := range map[string]string{"env.json": envJSON, "compose.yaml": "# generated\n"} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
			b.t.Fatal(err)
		}
	}
	if _, err := sshkey.EnsureMachineKey(filepath.Join(b.legacy, "id_ed25519"), "bulkhead@host"); err != nil {
		b.t.Fatal(err)
	}
	return dir
}

func (b *bench) write(path, content string) {
	b.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		b.t.Fatal(err)
	}
}

// noVolumeRemoved is the property everything here is built around.
func (b *bench) noVolumeRemoved(before map[string]bool) {
	b.t.Helper()
	for name := range before {
		if !b.engine.Volumes[name] {
			b.t.Errorf("the volume %s is gone", name)
		}
	}
	for _, call := range b.engine.Calls {
		if strings.HasPrefix(call, "rm-volume") {
			b.t.Errorf("a volume was removed: %s", call)
		}
	}
}

const legacyWeb = `{"type": "local", "sshPort": 22001, "publish": false, "restart": "unless-stopped", "authorizedKeys": [], "gpu": "off", "cdiDevices": [], "dockerSocket": false}`

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestMigrateTakesOverARunningEnvironment(t *testing.T) {
	b := newBench(t)
	oldDir := b.legacyEnv("web", legacyWeb)
	b.engine.AddLegacy("web", true)
	volumes := maps.Clone(b.engine.Volumes)

	b.stdin = "y\n"
	out := b.ok("migrate", "web")
	for _, part := range []string{
		"used as they are — not copied, not removed", "bulkhead-web_workspace (exists)",
		"it is running", "Migrated \"web\"", "it is up in the new containers",
		"bulkhead-web_github (exists)",
		"bh migrate --undo web",
	} {
		if !strings.Contains(out, part) {
			t.Errorf("output lacks %q:\n%s", part, out)
		}
	}
	b.noVolumeRemoved(volumes)

	// bh has it now, under the old name as its id, on the old port.
	web := b.env("web")
	if web.ID != "web" || web.SSHPort != 22001 || web.Restart != "unless-stopped" {
		t.Errorf("web = %+v", web)
	}
	// The TypeScript version no longer sees it; its files are all still there.
	retired := filepath.Join(b.legacy, "environments.migrated", "web")
	if exists(oldDir) || !exists(filepath.Join(retired, "env.json")) || !exists(filepath.Join(retired, "compose.yaml")) {
		t.Errorf("old settings: %s exists=%v, retired env.json=%v", oldDir, exists(oldDir), exists(filepath.Join(retired, "env.json")))
	}
	if names, _ := (config.LegacyStore{Dir: b.legacy}).Names(); len(names) != 0 {
		t.Errorf("the TypeScript version still lists %v", names)
	}
	// The same machine key keeps working: bh took a copy.
	oldKey, _ := os.ReadFile(filepath.Join(b.legacy, "id_ed25519"))
	newKey, _ := os.ReadFile(filepath.Join(b.dir, "id_ed25519"))
	if len(oldKey) == 0 || string(oldKey) != string(newKey) {
		t.Error("the machine key was not carried over")
	}
	if info, err := os.Stat(filepath.Join(b.dir, "id_ed25519")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the copied key's mode: %v", info.Mode().Perm())
	}

	// The old containers are gone, the new ones run.
	for _, name := range []string{"bulkhead-web-holder-1", "bulkhead-web-dev-1"} {
		if _, there := b.engine.Containers[name]; there {
			t.Errorf("%s is still there", name)
		}
	}
	if list := b.ok("list"); !strings.Contains(list, "running") {
		t.Errorf("list:\n%s", list)
	}
}

func TestMigrateAsksFirstAndTakesNoForAnAnswer(t *testing.T) {
	b := newBench(t)
	oldDir := b.legacyEnv("web", legacyWeb)
	b.engine.AddLegacy("web", true)

	for _, answer := range []string{"n\n", "", "web\n"} {
		b.stdin = answer
		if stderr := b.fails(1, "migrate", "web"); !strings.Contains(stderr, "nothing was changed") {
			t.Errorf("answer %q: %s", answer, stderr)
		}
	}
	if calls := b.engine.Lifecycle(); len(calls) != 0 {
		t.Errorf("Docker was changed: %v", calls)
	}
	if !exists(oldDir) || exists(b.dir) {
		t.Errorf("directories were changed (old there: %v, bh's created: %v)", exists(oldDir), exists(b.dir))
	}
}

func TestMigrateRefusesWhatItCannotCarry(t *testing.T) {
	tests := []struct {
		name  string
		setup func(b *bench, dir string)
		json  string
		want  string
	}{
		{"an override", func(b *bench, dir string) { b.write(filepath.Join(dir, "compose.override.yaml"), "services: {}\n") }, legacyWeb, "compose.override.yaml"},
		{"a key that is not registered", nil, `{"type": "local", "sshPort": 22001, "authorizedKeys": ["SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"]}`, "keys.json"},
		{"a name bh already uses", func(b *bench, _ string) { b.ok("create", "web", "--port", "23000") }, legacyWeb, `already has an environment named "web"`},
		{"a port bh already uses", func(b *bench, _ string) { b.ok("create", "other", "--port", "22001") }, legacyWeb, "ssh port 22001 is already used"},
		{"an earlier migration of the same name", func(b *bench, _ string) {
			os.MkdirAll(filepath.Join(b.legacy, "environments.migrated", "web"), 0o755)
		}, legacyWeb, "already exists"},
		{"settings it cannot read", nil, `{"type": "local", "sshPort": 22001, "surprise": true}`, "unknown field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newBench(t)
			dir := b.legacyEnv("web", tt.json)
			if tt.setup != nil {
				tt.setup(b, dir)
			}
			b.engine.AddLegacy("web", true)
			b.engine.Calls = nil
			volumes := maps.Clone(b.engine.Volumes)

			if stderr := b.fails(1, "migrate", "web", "--yes"); !strings.Contains(stderr, tt.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr, tt.want)
			}
			if calls := b.engine.Lifecycle(); len(calls) != 0 {
				t.Errorf("Docker was changed: %v", calls)
			}
			if !exists(filepath.Join(dir, "env.json")) {
				t.Error("the old settings were moved")
			}
			b.noVolumeRemoved(volumes)
		})
	}
}

// The new containers do not come up: everything is as before, and it shows.
func TestMigrateThatFailsLeavesTheEnvironmentWithTheOldVersion(t *testing.T) {
	b := newBench(t)
	oldDir := b.legacyEnv("web", legacyWeb)
	b.engine.AddLegacy("web", true)
	b.engine.ExitOnStart, b.engine.ExitOnStartName = true, "bulkhead-web-holder"
	volumes := maps.Clone(b.engine.Volumes)

	stderr := b.fails(1, "migrate", "web", "--yes")
	if !strings.Contains(stderr, "old containers were started again") {
		t.Errorf("stderr: %s", stderr)
	}
	b.noVolumeRemoved(volumes)
	if !exists(filepath.Join(oldDir, "env.json")) || exists(filepath.Join(b.legacy, "environments.migrated")) {
		t.Error("the old settings were moved although nothing was migrated")
	}
	if cfg, err := (config.Store{Dir: b.dir}).Load(); err != nil || len(cfg.Envs) != 0 {
		t.Errorf("bh lists environments after a failed migration: %+v, %v", cfg, err)
	}
	for _, name := range []string{"bulkhead-web-holder-1", "bulkhead-web-dev-1"} {
		if c := b.engine.Containers[name]; c == nil || c.Status != "running" {
			t.Errorf("%s is not running again", name)
		}
	}

	// Once whatever was wrong is fixed, it just works.
	b.engine.ExitOnStart = false
	b.ok("migrate", "web", "--yes")
	b.noVolumeRemoved(volumes)
}

func TestMigrateUndoHandsItBackAndKeepsTheVolumes(t *testing.T) {
	b := newBench(t)
	oldDir := b.legacyEnv("web", legacyWeb)
	b.engine.AddLegacy("web", true)
	b.ok("migrate", "web", "--yes")
	volumes := maps.Clone(b.engine.Volumes)
	b.engine.Calls = nil

	// Asked first here too.
	b.stdin = "\n"
	b.fails(1, "migrate", "--undo", "web")
	if len(b.engine.Lifecycle()) != 0 || exists(oldDir) {
		t.Fatal("an unconfirmed undo changed something")
	}

	out := b.ok("migrate", "--undo", "web", "--yes")
	if !strings.Contains(out, "Handed \"web\" back") || !strings.Contains(out, "volume prune") {
		t.Errorf("output:\n%s", out)
	}
	b.noVolumeRemoved(volumes)
	if len(volumes) < 4 {
		t.Fatalf("volumes: %v", volumes)
	}
	if len(b.engine.Containers) != 0 {
		t.Errorf("containers left: %v", slices.Collect(maps.Keys(b.engine.Containers)))
	}
	if !exists(filepath.Join(oldDir, "env.json")) || exists(filepath.Join(b.legacy, "environments.migrated", "web")) {
		t.Error("the old settings are not back in place")
	}
	if cfg, _ := (config.Store{Dir: b.dir}).Load(); len(cfg.Envs) != 0 {
		t.Errorf("bh still lists it: %+v", cfg.Envs)
	}

	// An environment bh made itself has nothing to be handed back to.
	b.ok("create", "own")
	if stderr := b.fails(1, "migrate", "--undo", "own", "--yes"); !strings.Contains(stderr, "did not come from the TypeScript version") {
		t.Errorf("stderr: %s", stderr)
	}
}

// bh already has a machine key of its own: the old key must still get in,
// or every existing ssh setup for the environment breaks.
func TestMigrateLetsTheOldMachineKeyIn(t *testing.T) {
	b := newBench(t)
	b.ok("create", "own")
	b.ok("up", "own") // makes bh's own machine key
	b.legacyEnv("web", `{"type": "local", "sshPort": 22050}`)
	b.engine.AddLegacy("web", false)

	b.ok("migrate", "web", "--yes")

	oldPublic, _ := os.ReadFile(filepath.Join(b.legacy, "id_ed25519.pub"))
	oldPrint, err := sshkey.Fingerprint(string(oldPublic))
	if err != nil {
		t.Fatal(err)
	}
	web := b.env("web")
	if !slices.Contains(web.AuthorizedKeys, oldPrint) {
		t.Errorf("the old machine key is not let in: %v", web.AuthorizedKeys)
	}
	store := config.Store{Dir: b.dir}
	registry, err := store.LoadKeys()
	if err != nil {
		t.Fatal(err)
	}
	if keys, err := config.ResolveKeys(registry, web); err != nil || len(keys) != 1 {
		t.Errorf("the reference does not resolve: %v, %v", keys, err)
	}
	// bh's own key was not replaced.
	ownPublic, _ := os.ReadFile(filepath.Join(b.dir, "id_ed25519.pub"))
	if string(ownPublic) == string(oldPublic) {
		t.Error("bh's own machine key was overwritten")
	}
	// It was stopped, so it stays stopped — with containers that hold the volumes.
	if out := b.ok("status", "web"); !strings.Contains(out, "state:    stopped") {
		t.Errorf("status:\n%s", out)
	}
}

func TestMigrateListAndRemote(t *testing.T) {
	b := newBench(t)
	if out := b.ok("migrate"); !strings.Contains(out, "has no environments") {
		t.Errorf("empty: %s", out)
	}
	b.legacyEnv("web", legacyWeb)
	b.write(filepath.Join(b.legacyEnv("gpu", `{"type": "local", "sshPort": 22002, "gpu": "cdi", "cdiDevices": ["nvidia.com/gpu=0"]}`), "compose.override.yaml"), "services: {}\n")
	b.legacyEnv("box", `{"type": "remote", "host": "example.com", "sshPort": 22}`)

	out := b.ok("migrate")
	rows := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) >= 3 {
			rows[fields[0]] = strings.Join(fields[2:], " ")
		}
	}
	if rows["web"] != "ready" || rows["box"] != "ready" || !strings.HasPrefix(rows["gpu"], "blocked: it has a compose.override.yaml") {
		t.Errorf("list:\n%s", out)
	}
	if len(b.engine.Calls) != 0 || exists(b.dir) {
		t.Error("listing changed something")
	}

	// A remote environment is settings only: Docker is not involved.
	b.ok("migrate", "box", "--yes")
	if len(b.engine.Calls) != 0 {
		t.Errorf("Docker was touched for a remote environment: %v", b.engine.Calls)
	}
	if box := b.env("box"); box.Host != "example.com" || box.Local() {
		t.Errorf("box = %+v", box)
	}
	if !exists(filepath.Join(b.legacy, "environments.migrated", "box", "env.json")) {
		t.Error("the old settings of box were not moved aside")
	}
}

// Interrupted between writing bh's settings and moving the old ones away:
// running it again finishes, and does not trip over its own first half.
func TestMigrateCanBeFinishedAfterAnInterruption(t *testing.T) {
	b := newBench(t)
	oldDir := b.legacyEnv("web", legacyWeb)
	b.engine.AddLegacy("web", true)
	b.ok("migrate", "web", "--yes")
	// Put the old directory back, as if the last step had not happened.
	if err := os.Rename(filepath.Join(b.legacy, "environments.migrated", "web"), oldDir); err != nil {
		t.Fatal(err)
	}
	volumes := maps.Clone(b.engine.Volumes)
	b.engine.Calls = nil

	out := b.ok("migrate", "web", "--yes")
	if !strings.Contains(out, "an earlier run was interrupted") {
		t.Errorf("output:\n%s", out)
	}
	b.noVolumeRemoved(volumes)
	if exists(oldDir) || len(b.engine.Containers) != 2 {
		t.Errorf("not finished: old dir there=%v, containers=%d", exists(oldDir), len(b.engine.Containers))
	}
}

func TestMigrateNeedsTwoDirectories(t *testing.T) {
	b := newBench(t)
	b.legacy = b.dir
	if stderr := b.fails(1, "migrate"); !strings.Contains(stderr, "same directory") {
		t.Errorf("stderr: %s", stderr)
	}
}

func TestMigrateBringsTheEnvironmentsFilesAndInitScript(t *testing.T) {
	b := newBench(t)
	dir := b.legacyEnv("web", `{"type": "local", "sshPort": 22001, "gpu": "legacy"}`)
	b.write(filepath.Join(dir, "dev.Dockerfile"), "FROM bulkhead-dev-base\nCOPY files/app.conf /etc/app.conf\n")
	os.MkdirAll(filepath.Join(dir, "files"), 0o755)
	b.write(filepath.Join(dir, "files", "app.conf"), "x=1\n")
	b.write(filepath.Join(dir, "github.json"), `{"preset": "read"}`)
	b.write(filepath.Join(b.legacy, "init.sh"), "echo theirs\n")
	b.engine.AddLegacy("web", false)

	out := b.ok("migrate", "web", "--yes")
	for _, part := range []string{"dev.Dockerfile", "files/app.conf", "github.json", "init.sh     copied"} {
		if !strings.Contains(out, part) {
			t.Errorf("output lacks %q:\n%s", part, out)
		}
	}
	store := config.Store{Dir: b.dir}
	for file, want := range map[string]string{
		"dev.Dockerfile": "FROM bulkhead-dev-base\nCOPY files/app.conf /etc/app.conf\n",
		"files/app.conf": "x=1\n",
		"github.json":    `{"preset": "read"}`,
	} {
		if data, _ := os.ReadFile(filepath.Join(store.EnvDir("web"), filepath.FromSlash(file))); string(data) != want {
			t.Errorf("%s = %q", file, data)
		}
	}
	// What the old version generated for itself stays behind.
	if exists(filepath.Join(store.EnvDir("web"), "compose.yaml")) {
		t.Error("compose.yaml was copied")
	}
	if data, _ := os.ReadFile(store.InitScript()); string(data) != "echo theirs\n" {
		t.Errorf("init.sh = %q", data)
	}
	if b.env("web").GPU != "legacy" {
		t.Errorf("gpu = %s", b.env("web").GPU)
	}

	// A second environment does not replace the init.sh bh now has.
	b.legacyEnv("api", `{"type": "local", "sshPort": 22002}`)
	b.write(filepath.Join(b.legacy, "init.sh"), "echo changed since\n")
	b.engine.AddLegacy("api", false)
	out = b.ok("migrate", "api", "--yes")
	if !strings.Contains(out, "bh already has its own") {
		t.Errorf("output does not mention the differing init.sh:\n%s", out)
	}
	if data, _ := os.ReadFile(store.InitScript()); string(data) != "echo theirs\n" {
		t.Errorf("init.sh was replaced: %q", data)
	}
}
