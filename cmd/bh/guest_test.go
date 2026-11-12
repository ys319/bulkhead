package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/image"
	"github.com/ys319/bulkhead/internal/plan"
)

// builds records the images that were built.
type builds struct {
	refs []string
	opts []image.Options
}

func (b *builds) Build(_ context.Context, ref string, src image.Source, opts image.Options, _ io.Writer) error {
	b.refs, b.opts = append(b.refs, ref), append(b.opts, opts)
	return nil
}

func devMounts(b *bench, id string) []mount.Mount {
	return b.engine.Created[plan.DevName(id)].Host.Mounts
}

func TestUpHandsInInitScriptAndCredentialVolumes(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.ok("up", "web")
	web := b.env("web")

	// init.sh is made from the template, and never replaced.
	script := filepath.Join(b.dir, "init.sh")
	data, err := os.ReadFile(script)
	if err != nil || !strings.HasPrefix(string(data), "#!/usr/bin/env zsh") {
		t.Fatalf("init.sh: %v\n%s", err, data)
	}
	if info, _ := os.Stat(script); info.Mode().Perm()&0o005 != 0o005 {
		t.Errorf("init.sh is not readable by the container's user: %v", info.Mode().Perm())
	}
	os.WriteFile(script, []byte("echo mine\n"), 0o755)
	b.ok("restart", "web")
	if data, _ := os.ReadFile(script); string(data) != "echo mine\n" {
		t.Errorf("init.sh was replaced: %q", data)
	}

	var bind, readOnly []string
	for _, m := range devMounts(b, web.ID) {
		if m.Type == mount.TypeBind {
			bind = append(bind, m.Source+" → "+m.Target)
		}
		if m.ReadOnly {
			readOnly = append(readOnly, m.Target)
		}
	}
	if !slices.Equal(bind, []string{script + " → /etc/bulkhead/init.sh"}) {
		t.Errorf("binds = %v", bind)
	}
	if !slices.Equal(readOnly, []string{"/etc/bulkhead/github", "/etc/bulkhead/tokens", "/etc/bulkhead/init.sh"}) {
		t.Errorf("read-only mounts = %v", readOnly)
	}
	for _, key := range []string{"github", "tokens"} {
		if !b.engine.Volumes[plan.Volume(web.ID, key)] {
			t.Errorf("the %s volume was not created", key)
		}
	}
}

func TestGPU(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	web := b.env("web")

	b.ok("configure", "web", "--gpu", "legacy")
	b.ok("up", "web")
	requests := b.engine.Created[plan.DevName(web.ID)].Host.DeviceRequests
	if len(requests) != 1 || requests[0].Count != -1 {
		t.Errorf("legacy: %+v", requests)
	}
	b.ok("down", "web")

	// A device the engine does not know stops the start before anything is
	// created or removed.
	b.engine.Devices = []string{"nvidia.com/gpu=0", "nvidia.com/gpu=1"}
	b.ok("configure", "web", "--gpu", "cdi", "--cdi-devices", "nvidia.com/gpu=7")
	b.engine.Calls = nil
	if err := b.fails(1, "up", "web"); !strings.Contains(err, "CDI device not found") || !strings.Contains(err, "available: nvidia.com/gpu=0, nvidia.com/gpu=1") {
		t.Errorf("unknown device: %s", err)
	}
	if calls := b.engine.Lifecycle(); len(calls) != 0 {
		t.Errorf("the failed start touched the containers: %v", calls)
	}

	b.ok("configure", "web", "--cdi-devices", "nvidia.com/gpu=1")
	b.engine.DeviceGroups = []int{44}
	b.ok("up", "web")
	host := b.engine.Created[plan.DevName(web.ID)].Host
	if len(host.DeviceRequests) != 1 || host.DeviceRequests[0].Driver != "cdi" || !slices.Equal(host.DeviceRequests[0].DeviceIDs, []string{"nvidia.com/gpu=1"}) {
		t.Errorf("cdi: %+v", host.DeviceRequests)
	}
	if !slices.Equal(host.GroupAdd, []string{"44"}) {
		t.Errorf("GroupAdd = %v", host.GroupAdd)
	}
	if !slices.Contains(b.engine.Calls, "probe-devices nvidia.com/gpu=1") {
		t.Errorf("the devices were not probed: %v", b.engine.Calls)
	}

	if out := b.ok("gpu", "list"); out != "nvidia.com/gpu=0\nnvidia.com/gpu=1\n" {
		t.Errorf("gpu list:\n%s", out)
	}
	b.engine.Devices = nil
	if err := b.fails(1, "gpu", "list"); !strings.Contains(err, "does not report CDI devices") {
		t.Errorf("gpu list without CDI: %s", err)
	}
}

func TestDevDockerfileBuildsTheEnvironmentsOwnImage(t *testing.T) {
	b := newBench(t)
	recorded := &builds{}
	b.builder = recorded
	b.ok("create", "web")
	web := b.env("web")
	store := config.Store{Dir: b.dir}

	// Without the file: the shared base image, no build (the engine has it).
	b.ok("up", "web")
	base := b.engine.Created[plan.DevName(web.ID)].Config.Image
	if !strings.HasPrefix(base, "bulkhead-dev-base:") || len(recorded.refs) != 0 {
		t.Fatalf("image = %s, builds = %v", base, recorded.refs)
	}

	// With it: an image of its own, built on the base under its plain name.
	os.WriteFile(store.DevDockerfile(web.ID), []byte("FROM bulkhead-dev-base\nRUN true\n"), 0o644)
	b.engine.MissingImage = func(ref string) bool { return strings.HasPrefix(ref, "bulkhead-dev-"+web.ID) }
	b.ok("up", "web")
	own := b.engine.Created[plan.DevName(web.ID)].Config.Image
	if !strings.HasPrefix(own, "bulkhead-dev-"+web.ID+":") || !slices.Equal(recorded.refs, []string{own}) {
		t.Errorf("image = %s, builds = %v", own, recorded.refs)
	}
	if b.engine.Tags["bulkhead-dev-base:latest"] != base {
		t.Errorf("tags = %v, want bulkhead-dev-base:latest → %s", b.engine.Tags, base)
	}

	// Editing the file gives another image; the same file does not build again.
	b.engine.MissingImage = func(ref string) bool { return ref != own && strings.HasPrefix(ref, "bulkhead-dev-"+web.ID) }
	b.ok("up", "web")
	if len(recorded.refs) != 1 {
		t.Errorf("an unchanged dev.Dockerfile was built again: %v", recorded.refs)
	}
	os.WriteFile(store.DevDockerfile(web.ID), []byte("FROM bulkhead-dev-base\nRUN false\n"), 0o644)
	b.ok("up", "web")
	if len(recorded.refs) != 2 || recorded.refs[1] == own {
		t.Errorf("builds after an edit = %v", recorded.refs)
	}
}

func TestBuildContextLeavesOutWhatIsBhs(t *testing.T) {
	for name, want := range map[string]bool{
		"env.json": true, "tokens.json": true, "github.json": true, "env.json.tmp": true, ".DS_Store": true,
		"dev.Dockerfile": false, "requirements.txt": false, "files/app.conf": false,
	} {
		if got := keptOutOfBuilds(name); got != want {
			t.Errorf("keptOutOfBuilds(%q) = %v", name, got)
		}
	}
}

func TestRestartRebuild(t *testing.T) {
	b := newBench(t)
	recorded := &builds{}
	b.builder = recorded
	b.ok("create", "web")
	b.ok("up", "web")

	b.ok("restart", "web", "--rebuild", "--pull", "--no-cache")
	if len(recorded.refs) != 1 || !strings.HasPrefix(recorded.refs[0], "bulkhead-dev-base:") || recorded.opts[0] != (image.Options{Pull: true, NoCache: true}) {
		t.Errorf("builds = %v %+v", recorded.refs, recorded.opts)
	}
	b.fails(2, "restart", "web", "--pull")

	// With a dev.Dockerfile the layer on top is built again too, without
	// pulling: its base exists on this machine only.
	web := b.env("web")
	os.WriteFile(config.Store{Dir: b.dir}.DevDockerfile(web.ID), []byte("FROM bulkhead-dev-base\n"), 0o644)
	recorded.refs, recorded.opts = nil, nil
	b.ok("restart", "web", "--rebuild", "--pull")
	if len(recorded.refs) != 2 || recorded.opts[0] != (image.Options{Pull: true}) || recorded.opts[1] != (image.Options{}) {
		t.Errorf("builds = %v %+v", recorded.refs, recorded.opts)
	}
}

func TestLogsMarksEachLineWithItsContainer(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	if err := b.fails(1, "logs", "web"); !strings.Contains(err, "no containers yet") {
		t.Errorf("before the first up: %s", err)
	}
	b.ok("up", "web")
	web := b.env("web")
	b.engine.Output[plan.HolderName(web.ID)] = "[holder] filter ready\r\n"
	b.engine.Output[plan.DevName(web.ID)] = "[dev] generated new ssh host key\nlistening" // no line end

	out := b.ok("logs", "web", "--no-follow", "--tail", "50")
	for _, line := range []string{"holder | [holder] filter ready\n", "dev    | [dev] generated new ssh host key\n", "dev    | listening\n"} {
		if !strings.Contains(out, line) {
			t.Errorf("logs lack %q:\n%q", line, out)
		}
	}
	if !slices.Contains(b.engine.Calls, "logs "+plan.DevName(web.ID)+" follow=false tail=50") {
		t.Errorf("calls = %v", b.engine.Calls)
	}
	// Following is the default.
	b.ok("logs", "web")
	if !slices.Contains(b.engine.Calls, "logs "+plan.DevName(web.ID)+" follow=true tail=0") {
		t.Errorf("calls = %v", b.engine.Calls)
	}
}

func TestInitRunsTheGuestHelper(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	if err := b.fails(1, "init", "web"); !strings.Contains(err, "not running") {
		t.Errorf("stopped: %s", err)
	}
	b.ok("up", "web")
	b.ok("init", "web")
	if last := b.engine.Execs[len(b.engine.Execs)-1]; !slices.Equal(last.Cmd, []string{"/usr/local/bin/bh-guest", "init"}) || last.User != "" || last.Privileged || last.TTY {
		t.Errorf("exec = %+v", last)
	}

	// A failing init.sh is passed through; an image without the helper is named.
	b.engine.ExecExit = 1
	b.fails(1, "init", "web")
	b.engine.ExecExit = 127
	if err := b.fails(1, "init", "web"); !strings.Contains(err, "bh restart web --rebuild") {
		t.Errorf("stale image: %s", err)
	}
}

func TestRootShell(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.ok("up", "web")

	if err := b.fails(1, "root-shell", "web"); !strings.Contains(err, "needs a terminal") {
		t.Errorf("without a terminal: %s", err)
	}
	b.typing = true
	_, _, stderr := b.run("root-shell", "web")
	if !strings.Contains(stderr, "full privileges") || b.raw != 1 {
		t.Errorf("stderr: %s (raw mode %d times)", stderr, b.raw)
	}
	spec := b.engine.Execs[len(b.engine.Execs)-1]
	if spec.User != "root" || !spec.Privileged || !spec.TTY || spec.WorkingDir != "/root" || !slices.Equal(spec.Cmd, []string{"zsh"}) {
		t.Errorf("exec = %+v", spec)
	}
	// Nothing the developer user can write to is on root's PATH.
	for _, entry := range spec.Env {
		if strings.HasPrefix(entry, "PATH=") && (strings.Contains(entry, "nix") || strings.Contains(entry, "/home")) {
			t.Errorf("root's %s", entry)
		}
	}
	if !slices.Contains(spec.Env, "HOME=/root") {
		t.Errorf("env = %v", spec.Env)
	}

	b.ok("down", "web")
	if err := b.fails(1, "root-shell", "web"); !strings.Contains(err, "not running") {
		t.Errorf("stopped: %s", err)
	}
}
