package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/doctor"
	"github.com/ys319/bulkhead/internal/engine"
	"github.com/ys319/bulkhead/internal/ops"
)

// fake is a Backend that keeps everything in memory and records what was
// asked of it.
type fake struct {
	snapshots []SnapshotEntry
	snap      Snapshot
	calls     []string
	vscode    bool
	fail      map[string]error // by the first word of a call
	devices   []string
}

func (f *fake) call(format string, args ...any) error {
	text := fmt.Sprintf(format, args...)
	f.calls = append(f.calls, text)
	word, _, _ := strings.Cut(text, " ")
	return f.fail[word]
}

func (f *fake) find(name string) *EnvState {
	for i := range f.snap.Envs {
		if f.snap.Envs[i].Env.Name == name {
			return &f.snap.Envs[i]
		}
	}
	return nil
}

func running(started time.Time) ops.Status {
	return ops.Status{
		Holder: &engine.Container{ID: "h", Status: "running", Health: "healthy", StartedAt: started},
		Dev:    &engine.Container{ID: "d", Status: "running", NetworkMode: "container:h", StartedAt: started.Add(time.Second)},
	}
}

func stopped() ops.Status {
	return ops.Status{Holder: &engine.Container{ID: "h", Status: "exited"}, Dev: &engine.Container{ID: "d", Status: "exited", NetworkMode: "container:h"}}
}

func (f *fake) Load(context.Context) (Snapshot, error) {
	snap := f.snap
	snap.Envs = slices.Clone(f.snap.Envs)
	snap.Keys = slices.Clone(f.snap.Keys)
	return snap, f.fail["load"]
}
func (f *fake) HasVSCode() bool { return f.vscode }
func (f *fake) Create(name, from string) error {
	if err := f.call("create %s from=%s", name, from); err != nil {
		return err
	}
	f.snap.Envs = append(f.snap.Envs, EnvState{Env: config.NewLocal("id-"+name, name, 22100+len(f.snap.Envs)), Known: true})
	return nil
}
func (f *fake) Rename(name, newName string) error {
	if err := f.call("rename %s %s", name, newName); err != nil {
		return err
	}
	f.find(name).Env.Name = newName
	return nil
}
func (f *fake) Up(_ context.Context, name string, progress io.Writer) error {
	fmt.Fprintln(progress, "building bulkhead-dev-base")
	if err := f.call("up %s", name); err != nil {
		return err
	}
	f.find(name).Status = running(time.Now())
	return nil
}
func (f *fake) Down(_ context.Context, name string) error {
	if err := f.call("down %s", name); err != nil {
		return err
	}
	f.find(name).Status = stopped()
	return nil
}
func (f *fake) Restart(_ context.Context, name string, _ io.Writer) error {
	return f.call("restart %s", name)
}
func (f *fake) Reset(_ context.Context, name string) error { return f.call("reset %s", name) }
func (f *fake) Snapshot(_ context.Context, name string, _ io.Writer) (string, error) {
	return "2026-11-13T090000Z", f.call("snapshot %s", name)
}
func (f *fake) Snapshots(string) ([]SnapshotEntry, error) { return f.snapshots, nil }
func (f *fake) Rollback(_ context.Context, name, snapshot string, _ io.Writer) (string, error) {
	return "2026-11-13T100000Z", f.call("rollback %s %s", name, snapshot)
}
func (f *fake) Archive(_ context.Context, name string, _ io.Writer) error {
	if err := f.call("archive %s", name); err != nil {
		return err
	}
	for i := range f.snap.Envs {
		if f.snap.Envs[i].Env.Name == name {
			f.snap.Envs[i].Env.Archived = "2026-11-13T090000Z.tar.gz"
			f.snap.Envs[i].Status = ops.Status{}
		}
	}
	return nil
}
func (f *fake) Unarchive(_ context.Context, name string, _ io.Writer) error {
	if err := f.call("unarchive %s", name); err != nil {
		return err
	}
	for i := range f.snap.Envs {
		if f.snap.Envs[i].Env.Name == name {
			f.snap.Envs[i].Env.Archived = ""
		}
	}
	return nil
}
func (f *fake) Destroy(_ context.Context, name string) error {
	if err := f.call("destroy %s", name); err != nil {
		return err
	}
	f.snap.Envs = slices.DeleteFunc(f.snap.Envs, func(state EnvState) bool { return state.Env.Name == name })
	return nil
}
func (f *fake) Configure(_ context.Context, name string, changes config.Changes) error {
	if err := f.call("configure %s locked=%s", name, strings.Join(changes.Locked(), ",")); err != nil {
		return err
	}
	state := f.find(name)
	state.Env = state.Env.With(changes)
	return nil
}
func (f *fake) RunInit(_ context.Context, name string, _ io.Writer) error {
	return f.call("init %s", name)
}
func (f *fake) Open(_ context.Context, name string) error { return f.call("open %s", name) }
func (f *fake) AddRemote(name, host string, port int) error {
	if err := f.call("add-remote %s %s:%d", name, host, port); err != nil {
		return err
	}
	f.snap.Envs = append(f.snap.Envs, EnvState{Env: config.NewRemote("id-"+name, name, host, port)})
	return nil
}
func (f *fake) ConfigureRemote(name string, host *string, port *int) error {
	return f.call("configure-remote %s %s:%d", name, *host, *port)
}
func (f *fake) RemoveRemote(name string) error             { return f.call("remove-remote %s", name) }
func (f *fake) Reach(_ context.Context, name string) error { return f.fail["reach"] }
func (f *fake) RegisterKey(input, label string) (config.KeyEntry, error) {
	if err := f.call("register-key %s", label); err != nil {
		return config.KeyEntry{}, err
	}
	entry := config.KeyEntry{Fingerprint: "SHA256:" + strings.Repeat("N", 43), PublicKey: input, Label: label}
	f.snap.Keys = append(f.snap.Keys, entry)
	return entry, nil
}
func (f *fake) RenameKey(fingerprint, label string) error {
	return f.call("rename-key %s %s", fingerprint[:10], label)
}
func (f *fake) RemoveKey(fingerprint string) ([]string, error) {
	return nil, f.call("remove-key %s", fingerprint[:10])
}
func (f *fake) AttachKey(name, fingerprint string) error {
	if err := f.call("attach %s %s", name, fingerprint[:10]); err != nil {
		return err
	}
	state := f.find(name)
	state.Env.AuthorizedKeys = append(state.Env.AuthorizedKeys, fingerprint)
	return nil
}
func (f *fake) DetachKey(name, fingerprint string) error {
	if err := f.call("detach %s %s", name, fingerprint[:10]); err != nil {
		return err
	}
	state := f.find(name)
	state.Env.AuthorizedKeys = slices.DeleteFunc(state.Env.AuthorizedKeys, func(fp string) bool { return fp == fingerprint })
	return nil
}
func (f *fake) MachineKey() (string, error) { return "ssh-ed25519 AAAAmachine bulkhead", nil }
func (f *fake) CDIDevices(context.Context) ([]string, bool, error) {
	return f.devices, f.devices != nil, nil
}
func (f *fake) Addresses() []Address {
	return []Address{{Host: "studio", Label: "hostname"}, {Host: "192.168.1.20", Label: "en0"}}
}
func (f *fake) Shell(name string) (*exec.Cmd, error) {
	return exec.Command("true"), f.call("shell %s", name)
}
func (f *fake) Self(args ...string) (*exec.Cmd, error) {
	return exec.Command("true"), f.call("self %s", strings.Join(args, " "))
}
func (f *fake) Editor(path string) (*exec.Cmd, error) {
	return exec.Command("true"), f.call("editor %s", path)
}
func (f *fake) InitScript() (string, error) { return "/cfg/init.sh", nil }
func (f *fake) DevDockerfile(name string) (string, error) {
	return "/cfg/" + name + "/dev.Dockerfile", nil
}
func (f *fake) TidyDevDockerfile(string) (bool, error) { return false, nil }
func (f *fake) Watch(ctx context.Context) <-chan bool {
	changes := make(chan bool)
	go func() { <-ctx.Done(); close(changes) }()
	return changes
}
func (f *fake) SetToken(_ context.Context, name, kind, tokenName, value string) (string, error) {
	if err := f.call("set-token %s %s %s len=%d", name, kind, tokenName, len(value)); err != nil {
		return "", err
	}
	state := f.find(name)
	state.Tokens = append(state.Tokens, Token{Kind: kind, Name: tokenName})
	return "saved " + tokenName, nil
}
func (f *fake) RemoveToken(_ context.Context, name, tokenName string) (string, error) {
	if err := f.call("remove-token %s %s", name, tokenName); err != nil {
		return "", err
	}
	state := f.find(name)
	state.Tokens = slices.DeleteFunc(state.Tokens, func(token Token) bool { return token.Name == tokenName })
	return "removed " + tokenName, nil
}
func (f *fake) TokenPage(kind string) string {
	if kind == config.TokenKindHF {
		return "https://hf.test/tokens"
	}
	return ""
}
func (f *fake) GitHubIssue(name, preset string) (string, string, error) {
	return "https://github.test/new?preset=" + preset, "bulkhead-" + name, f.call("github-issue %s %s", name, preset)
}
func (f *fake) SetGitHub(_ context.Context, name, token, preset, tokenName string) (string, error) {
	if err := f.call("set-github %s %s %s", name, preset, tokenName); err != nil {
		return "", err
	}
	state := f.find(name)
	state.Tokens = append([]Token{{Kind: "github", Name: "GH_TOKEN", Summary: preset + " · octocat · 2 repos · expires in 30d"}}, state.Tokens...)
	return "GitHub token set", nil
}
func (f *fake) RevokeGitHub(_ context.Context, name string) (string, error) {
	if err := f.call("revoke-github %s", name); err != nil {
		return "", err
	}
	state := f.find(name)
	state.Tokens = slices.DeleteFunc(state.Tokens, func(token Token) bool { return token.Kind == "github" })
	return "revoked", nil
}
func (f *fake) OpenURL(address string) error { return f.call("open-url %s", address) }
func (f *fake) DiskUsage(context.Context) (map[string]int64, error) {
	return map[string]int64{
		"bulkhead-id-web_workspace": 1_200_000_000, "bulkhead-id-web_home": 513_000_000,
		"bulkhead-id-web_nix": 2_500_000_000, "bulkhead-id-web_hostkeys": 4096,
	}, f.fail["disk"]
}
func (f *fake) FollowLogs(ctx context.Context, name string, _ int, out io.Writer) error {
	fmt.Fprintf(out, "[dev] hello from %s\n", name)
	<-ctx.Done()
	return nil
}

const (
	laptopKey = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	phoneKey  = "SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
)

// screenOf is a Model on a fake with two local environments (web running,
// api stopped and never started), a remote one, and two registered keys.
type screenOf struct {
	t *testing.T
	*Model
	backend *fake
}

func start(t *testing.T) *screenOf {
	web := config.NewLocal("id-web", "web", 22001)
	web.AuthorizedKeys = []string{laptopKey}
	api := config.NewLocal("id-api", "api", 22002)
	backend := &fake{vscode: true, fail: map[string]error{}, snap: Snapshot{
		Envs: []EnvState{
			{Env: api, Known: true},
			{Env: config.NewRemote("id-box", "box", "studio.test", 22001)},
			{Env: web, Known: true, Status: running(time.Now().Add(-3*time.Hour - time.Minute))},
		},
		Keys: []config.KeyEntry{
			{Fingerprint: laptopKey, PublicKey: "ssh-ed25519 AAAAlaptop me@laptop", Label: "laptop"},
			{Fingerprint: phoneKey, PublicKey: "ssh-ed25519 AAAAphone", Label: "phone"},
		},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &screenOf{t: t, Model: New(ctx, backend), backend: backend}
	s.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	s.settle(s.reload(true))
	s.settle(s.measure())
	return s
}

// settle runs a command and feeds what it returns back in, as the program
// would. Commands that wait (timers, the watch) are dropped.
func (s *screenOf) settle(cmd tea.Cmd) {
	s.t.Helper()
	if cmd == nil {
		return
	}
	result := make(chan tea.Msg, 1)
	go func() { result <- cmd() }()
	select {
	case msg := <-result:
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, each := range batch {
				s.settle(each)
			}
			return
		}
		if msg != nil {
			_, next := s.Update(msg)
			s.settle(next)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

var named = map[string]rune{
	"enter": tea.KeyEnter, "esc": tea.KeyEscape, "up": tea.KeyUp, "down": tea.KeyDown,
	"left": tea.KeyLeft, "right": tea.KeyRight, "space": tea.KeySpace, "backspace": tea.KeyBackspace, "tab": tea.KeyTab,
}

// press sends keys: the names above, or text typed character by character.
func (s *screenOf) press(keys ...string) *screenOf {
	s.t.Helper()
	for _, key := range keys {
		if code, special := named[key]; special {
			_, cmd := s.Update(tea.KeyPressMsg{Code: code})
			s.settle(cmd)
			continue
		}
		for _, r := range key {
			_, cmd := s.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
			s.settle(cmd)
		}
	}
	return s
}

// onto moves the cursor to the row with the label.
func (s *screenOf) onto(label string) *screenOf {
	s.t.Helper()
	for _, r := range s.rows {
		if r.selectable() && r.label == label {
			s.cursor = r.id
			return s
		}
	}
	s.t.Fatalf("no row %q", label)
	return s
}

// pick moves to the menu item with the label and chooses it.
func (s *screenOf) pick(label string) *screenOf {
	s.t.Helper()
	d, ok := s.top().(*menu)
	if !ok {
		s.t.Fatalf("pick %q: no menu is open (top is %T)", label, s.top())
	}
	for i, it := range d.items(s.Model) {
		if it.label == label || strings.HasSuffix(it.label, "] "+label) {
			d.cursor = i
			return s.press("enter")
		}
	}
	s.t.Fatalf("the menu %q has no item %q: %v", d.name, label, s.labels())
	return s
}

// labels are the items of the open menu, separators as "---".
func (s *screenOf) labels() []string {
	d, ok := s.top().(*menu)
	if !ok {
		return nil
	}
	var labels []string
	for _, it := range d.items(s.Model) {
		if it.separator {
			labels = append(labels, "---")
		} else {
			labels = append(labels, it.label)
		}
	}
	return labels
}

func (s *screenOf) text() string { return sanitizeLines(strings.Join(s.screen(), "\n")) }

func (s *screenOf) shows(parts ...string) {
	s.t.Helper()
	text := s.text()
	for _, part := range parts {
		if !strings.Contains(text, part) {
			s.t.Errorf("the screen lacks %q:\n%s", part, text)
		}
	}
}

func (s *screenOf) called(call string) {
	s.t.Helper()
	if !slices.Contains(s.backend.calls, call) {
		s.t.Errorf("no call %q; calls: %v", call, s.backend.calls)
	}
}

func TestListIsInSectionsEachWithItsAddRow(t *testing.T) {
	s := start(t)
	var labels []string
	for _, r := range s.rows {
		labels = append(labels, r.label)
	}
	want := []string{"Local", "api", "web", "+ Create environment", "", "Remote", "box", "+ Add remote environment", "",
		"Keys", "laptop", "phone", "+ Register key", "", "System", "Settings", "Machine key"}
	if !slices.Equal(labels, want) {
		t.Errorf("rows = %q", labels)
	}
	// The cursor starts on the first thing that can be chosen and skips
	// headings and blank lines.
	if s.selected().label != "api" {
		t.Errorf("cursor on %q", s.selected().label)
	}
	s.press("down", "down", "down")
	if s.selected().label != "box" {
		t.Errorf("after three downs: %q", s.selected().label)
	}
	s.press("up", "up", "up", "up", "up")
	if s.selected().label != "api" {
		t.Errorf("at the top: %q", s.selected().label)
	}
}

func TestNameColorsShowState(t *testing.T) {
	now := time.Now()
	env := func(edit func(*EnvState)) EnvState {
		state := EnvState{Env: config.NewLocal("e", "e", 22001), Known: true}
		edit(&state)
		return state
	}
	half := running(now)
	half.Dev = nil
	tests := []struct {
		name  string
		state EnvState
		reach reachState
		want  style
	}{
		{"never started", env(func(*EnvState) {}), reachState{}, style{color: gray}},
		{"running", env(func(s *EnvState) { s.Status = running(now) }), reachState{}, style{color: green}},
		{"half running", env(func(s *EnvState) { s.Status = half }), reachState{}, style{color: red}},
		{"not isolated, running", env(func(s *EnvState) { s.Status = running(now); s.Env.DockerSocket = true }), reachState{}, style{color: magenta}},
		{"not isolated, stopped", env(func(s *EnvState) { s.Status = stopped(); s.Env.DockerSocket = true }), reachState{}, style{color: magenta, dim: true}},
		{"engine unreachable", env(func(s *EnvState) { s.Known = false }), reachState{}, style{color: gray}},
		{"remote, unchecked", EnvState{Env: config.NewRemote("r", "r", "h", 1)}, reachState{}, style{color: gray}},
		{"remote, reached", EnvState{Env: config.NewRemote("r", "r", "h", 1)}, reachState{checked: true}, style{color: green}},
		{"remote, failed", EnvState{Env: config.NewRemote("r", "r", "h", 1)}, reachState{checked: true, problem: "refused"}, style{color: red}},
	}
	for _, tt := range tests {
		if got := envStyle(tt.state, tt.reach); got != tt.want {
			t.Errorf("%s: %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

func TestEnvironmentMenuFollowsTheState(t *testing.T) {
	s := start(t)
	s.onto("api").press("enter")
	if got, want := s.labels(), []string{"Start", "---", "Settings", "Maintenance", "---", "Replicate", "Rename", "Destroy"}; !slices.Equal(got, want) {
		t.Errorf("never started: %q", got)
	}
	s.press("esc").onto("web").press("enter")
	want := []string{"Stop", "Reboot", "---", "Open shell", "Open in VS Code", "---", "Settings", "Maintenance", "---", "Replicate", "Rename", "Reset", "Destroy"}
	if got := s.labels(); !slices.Equal(got, want) {
		t.Errorf("running: %q", got)
	}
	// Without VS Code the item is not offered.
	s.backend.vscode = false
	if slices.Contains(s.labels(), "Open in VS Code") {
		t.Error("Open in VS Code is offered without the code command")
	}

	s.pick("Maintenance")
	if got, want := s.labels(), []string{"Root Shell", "Logs", "Edit Dockerfile", "Rebuild", "Rerun init.sh", "---", "Snapshot", "Roll back", "Archive"}; !slices.Equal(got, want) {
		t.Errorf("maintenance, running: %q", got)
	}
	s.press("esc", "esc").onto("api").press("enter").pick("Maintenance")
	if got, want := s.labels(), []string{"Edit Dockerfile"}; !slices.Equal(got, want) {
		t.Errorf("maintenance, never started: %q", got)
	}

	s.press("esc", "esc").onto("box").press("enter")
	if got, want := s.labels(), []string{"Open shell", "---", "Configure", "Rename", "---", "Remove"}; !slices.Equal(got, want) {
		t.Errorf("remote: %q", got)
	}
}

func TestStartAndStopRunBehindABusyDialog(t *testing.T) {
	s := start(t)
	s.onto("api").press("enter").pick("Start")
	s.called("up api")
	if s.top() != nil {
		t.Errorf("a dialog is still open: %T", s.top())
	}
	s.shows(`"api" is up`)
	// The list was read again: the environment now offers Stop.
	s.press("enter")
	if s.labels()[0] != "Stop" {
		t.Errorf("after the start: %q", s.labels())
	}
	s.pick("Stop")
	s.called("down api")

	// A failure shows in red with the last output; m opens it in full.
	s.backend.fail["up"] = errors.New("the holder did not become healthy")
	s.press("enter").pick("Start")
	s.shows("the holder did not become healthy", "(+2 lines)")
	s.press("m")
	s.shows("building bulkhead-dev-base")
	s.press("esc")
	if s.note == nil {
		t.Error("closing the full message dropped it")
	}
	s.press("down")
	if s.note != nil {
		t.Error("the message stayed after a key in the list")
	}
}

func TestRebootAndResetAsk(t *testing.T) {
	s := start(t)
	s.onto("web").press("enter").pick("Reboot")
	s.shows("Everything outside the volumes")
	// No is where the cursor starts; Enter goes back to the menu.
	s.press("enter")
	if len(s.backend.calls) != 0 || s.labels() == nil {
		t.Errorf("calls = %v, top = %T", s.backend.calls, s.top())
	}
	s.pick("Reboot").press("right", "enter")
	s.called("restart web")

	s.press("enter").pick("Reset").press("esc")
	if slices.Contains(s.backend.calls, "reset web") {
		t.Error("reset ran without a yes")
	}
	s.pick("Reset").press("left", "enter")
	s.called("reset web")
}

func TestDestroyWantsTheNameTyped(t *testing.T) {
	s := start(t)
	s.onto("web").press("enter").pick("Destroy")
	s.press("wrong", "enter")
	s.shows("that is not its name")
	if len(s.backend.calls) != 0 {
		t.Errorf("calls = %v", s.backend.calls)
	}
	s.press("backspace", "backspace", "backspace", "backspace", "backspace", "web", "enter")
	s.called("destroy web")
	for _, r := range s.rows {
		if r.label == "web" {
			t.Error("web is still listed")
		}
	}
}

func TestCreateValidatesAndSelectsTheNewRow(t *testing.T) {
	s := start(t)
	s.onto("+ Create environment").press("enter", "Bad Name", "enter")
	s.shows("lowercase letters")
	s.press("ctrl+u")
	d := s.top().(*input)
	d.value = nil
	s.press("web", "enter")
	s.shows("already exists")
	d.value = nil
	s.press("new", "enter")
	s.called("create new from=")
	if s.selected().label != "new" {
		t.Errorf("cursor on %q", s.selected().label)
	}

	s.onto("web").press("enter").pick("Replicate").press("copy", "enter")
	s.called("create copy from=web")
}

func TestLockedSettingsAreGrayWhileRunning(t *testing.T) {
	s := start(t)
	s.onto("web").press("enter").pick("Settings").pick("SSH")
	s.shows("Open to LAN", "off", "22001", "Authorized Keys")
	s.pick("Open to LAN")
	s.shows(`stop "web" to change Open to LAN`)
	if len(s.backend.calls) != 0 {
		t.Errorf("calls = %v", s.backend.calls)
	}

	// Stopped: saved at once, and the menu stays with the new value.
	s.press("esc", "esc", "esc").onto("api").press("enter").pick("Settings").pick("SSH").pick("Open to LAN")
	s.called("configure api locked=publish")
	if got := s.labels(); !slices.Contains(got, "Copy address") {
		t.Errorf("after opening to the LAN: %q", got)
	}
	s.pick("Port").press("backspace", "backspace", "backspace", "backspace", "backspace", "abc", "enter")
	s.shows("a number between 1 and 65535")
	s.top().(*input).value = []rune("23000")
	s.press("enter")
	if s.backend.find("api").Env.SSHPort != 23000 {
		t.Errorf("port = %d", s.backend.find("api").Env.SSHPort)
	}
	s.pick("Copy address").pick("192.168.1.20")
	s.shows("192.168.1.20:23000/api")
}

func TestRuntimeFormSavesTogether(t *testing.T) {
	s := start(t)
	s.backend.devices = []string{"nvidia.com/gpu=0", "nvidia.com/gpu=1"}
	s.onto("api").press("enter").pick("Settings").pick("Runtime")
	form := s.top().(*menu)
	// Restart: unless-stopped → no; GPU: off → legacy → cdi.
	form.cursor = 0
	s.press("space")
	form.cursor = 1
	s.press("right", "right")
	if got := s.labels(); !slices.Contains(got, "Attach GPU") {
		t.Fatalf("with gpu cdi: %q", got)
	}
	// Without a device it does not save.
	s.pick("Save and close")
	s.shows("choose at least one device")
	if len(s.backend.calls) != 0 {
		t.Errorf("calls = %v", s.backend.calls)
	}
	s.pick("Attach GPU").pick("nvidia.com/gpu=1").pick("Done").pick("Save and close")
	s.called("configure api locked=restart,gpu,cdiDevices,dockerSocket")
	api := s.backend.find("api").Env
	if api.Restart != "no" || api.GPU != "cdi" || !slices.Equal(api.CDIDevices, []string{"nvidia.com/gpu=1"}) || api.DockerSocket {
		t.Errorf("api = %+v", api)
	}

	// Running: it opens, but nothing in it changes.
	s.onto("web").press("enter").pick("Settings").pick("Runtime").press("space")
	s.shows(`stop "web" to change Restart`)
}

func TestAuthorizedKeys(t *testing.T) {
	s := start(t)
	s.onto("web").press("enter").pick("Settings").pick("SSH").pick("Authorized Keys")
	if got, want := s.labels(), []string{"Detach 1: laptop", "+ Attach key"}; !slices.Equal(got, want) {
		t.Errorf("keys of web: %q", got)
	}
	s.pick("+ Attach key").pick("phone")
	s.called("attach web SHA256:BBB")
	s.shows("reboot to apply")
	s.pick("Detach 1: laptop").press("right", "enter")
	s.called("detach web SHA256:AAA")
	if got, want := s.labels(), []string{"Detach 1: phone", "+ Attach key"}; !slices.Equal(got, want) {
		t.Errorf("keys of web afterwards: %q", got)
	}
}

func TestKeyMenu(t *testing.T) {
	s := start(t)
	s.onto("laptop")
	s.shows("fingerprint: "+laptopKey, "environments: web")
	s.press("enter").pick("Environments")
	if got, want := s.labels(), []string{"[ ] api", "[x] web", "---", "Save and close"}; !slices.Equal(got, want) {
		t.Errorf("environments: %q", got)
	}
	s.pick("api").pick("web").pick("Save and close")
	s.called("attach api SHA256:AAA")
	s.called("detach web SHA256:AAA")

	s.onto("laptop").press("enter").pick("Remove")
	s.shows("detached from api first")
	s.press("right", "enter")
	s.called("remove-key SHA256:AAA")
}

func TestRegisterKeyRefusesWhatIsNotAKey(t *testing.T) {
	s := start(t)
	s.onto("+ Register key").press("enter")
	s.Update(tea.PasteMsg{Content: "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n"})
	s.press("enter")
	s.shows("PRIVATE key")
	s.top().(*input).value = nil
	s.Update(tea.PasteMsg{Content: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ7 me@tablet\n"})
	s.press("enter")
	// The comment is offered as the name.
	if got := string(s.top().(*input).value); got != "me@tablet" {
		t.Errorf("offered name = %q", got)
	}
	s.press("enter")
	s.called("register-key me@tablet")
	if s.selected().label != "me@tablet" {
		t.Errorf("cursor on %q", s.selected().label)
	}
}

func TestAddRemote(t *testing.T) {
	s := start(t)
	s.onto("+ Add remote environment").press("enter", "studio.test/main", "enter")
	s.shows("expected host:port/name")
	s.top().(*input).value = []rune("studio.test:22005/main")
	s.press("enter", "enter")
	s.called("add-remote main studio.test:22005")
	if s.selected().label != "main" {
		t.Errorf("cursor on %q", s.selected().label)
	}
}

func TestShellOuts(t *testing.T) {
	s := start(t)
	s.onto("web").press("enter").pick("Open shell")
	s.called("shell web")
	s.press("enter").pick("Maintenance").pick("Root Shell")
	s.called("self root-shell web")
	s.press("enter").pick("Maintenance").pick("Logs")
	s.called("self logs web")
	s.press("enter").pick("Maintenance").pick("Rebuild").press("right", "enter")
	s.called("self restart web --rebuild --pull --no-cache")
	s.press("enter").pick("Maintenance").pick("Edit Dockerfile")
	s.called("editor /cfg/web/dev.Dockerfile")
	s.onto("Settings").press("enter").pick("Edit init.sh")
	s.called("editor /cfg/init.sh")
}

func TestDetailOfARunningEnvironment(t *testing.T) {
	s := start(t)
	s.onto("web")
	s.settle(s.tendLogs())
	time.Sleep(20 * time.Millisecond)
	s.shows("state: running, up 3h", "restart: unless-stopped", "address: 127.0.0.1:22001", "alias: bulkhead-web",
		"keys: this machine + 1", "Logs", "[dev] hello from web",
		"disk: 4.2GB (workspace 1.2GB, home 513MB, nix 2.5GB)")
	s.onto("box")
	s.shows("box (remote)", "address: studio.test:22001")
}

func TestQuitAsksAndCtrlCDoesNot(t *testing.T) {
	s := start(t)
	s.press("q")
	s.shows("Leave bh?")
	s.press("esc")
	if s.top() != nil {
		t.Error("the question stayed open")
	}
	_, cmd := s.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("ctrl+c did nothing")
	}
	if _, quit := cmd().(tea.QuitMsg); !quit {
		t.Error("ctrl+c did not quit")
	}
}

func TestAStaleReloadDoesNotReplaceANewerOne(t *testing.T) {
	s := start(t)
	old := s.reload(false) // asked first, answers last
	s.backend.snap.Envs = s.backend.snap.Envs[:1]
	stale := old()
	s.settle(s.reload(false))
	s.Update(stale)
	if len(s.snap.Envs) != 1 {
		t.Errorf("the stale answer was taken: %d environments", len(s.snap.Envs))
	}
}

func TestScreenFitsTheTerminal(t *testing.T) {
	s := start(t)
	for _, size := range [][2]int{{80, 20}, {110, 30}, {200, 50}} {
		s.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		s.onto("web").press("enter")
		lines := s.screen()
		if len(lines) != size[1] {
			t.Errorf("%dx%d: %d lines", size[0], size[1], len(lines))
		}
		for i, line := range lines {
			if w := width(line); w > size[0] {
				t.Errorf("%dx%d: line %d is %d cells wide", size[0], size[1], i, w)
			}
		}
		s.press("esc")
	}
}

func TestOverlayKeepsWhatIsBeside(t *testing.T) {
	base := []string{"0123456789", paint(red, "abcdefghij"), "0123456789"}
	got := overlay(base, []string{"XX"}, 4, 1)
	if plainText := sanitize(got[1]); plainText != "abcdXXghij" {
		t.Errorf("row 1 = %q", plainText)
	}
	if got[0] != base[0] || got[2] != base[2] {
		t.Error("other rows changed")
	}
}

func TestSanitizeDropsEscapes(t *testing.T) {
	if got := sanitize("ok \x1b[31mred\x1b[0m \x07bell\ttab"); got != "ok red bell tab" {
		t.Errorf("sanitize = %q", got)
	}
}

func TestPicker(t *testing.T) {
	p := &picker{title: "Start which environment?", choices: []Choice{{Name: "api", Note: "stopped"}, {Name: "web", Note: "running"}}}
	send := func(code rune) tea.Cmd {
		_, cmd := p.Update(tea.KeyPressMsg{Code: code})
		return cmd
	}
	if text := sanitizeLines(p.View().Content); !strings.Contains(text, "❯ api  stopped") || !strings.Contains(text, "  web  running") {
		t.Errorf("view:\n%s", text)
	}
	send(tea.KeyUp)
	send(tea.KeyDown)
	send(tea.KeyDown)
	if cmd := send(tea.KeyEnter); cmd == nil || p.picked != "web" {
		t.Errorf("picked %q", p.picked)
	}

	p = &picker{choices: []Choice{{Name: "api"}}}
	if cmd := send(tea.KeyEscape); cmd == nil || p.picked != "" {
		t.Errorf("after esc: picked %q", p.picked)
	}
}

func TestHumanBytes(t *testing.T) {
	for size, want := range map[int64]string{0: "0B", 999: "999B", 4096: "4.1kB", 513_000_000: "513MB", 1_200_000_000: "1.2GB", 25_000_000_000: "25GB"} {
		if got := humanBytes(size); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", size, got, want)
		}
	}
	if _, has := DiskSummary("nope", map[string]int64{"bulkhead-id-web_home": 1}); has {
		t.Error("an environment without volumes got a disk summary")
	}
}

func TestTokensMenu(t *testing.T) {
	s := start(t)
	s.onto("web").press("enter").pick("Settings")
	if got, want := s.labels(), []string{"Runtime", "SSH", "Tokens"}; !slices.Equal(got, want) {
		t.Errorf("settings: %q", got)
	}
	s.pick("Tokens")
	if got, want := s.labels(), []string{"GitHub", "+ Add Hugging Face token", "+ Add custom token"}; !slices.Equal(got, want) {
		t.Errorf("tokens: %q", got)
	}

	// A custom token: its name is checked before the value is asked for,
	// and the value never shows.
	s.pick("+ Add custom token").press("PATH", "enter")
	if _, still := s.top().(*input); !still {
		t.Fatal("a reserved name was accepted")
	}
	s.top().(*input).value = []rune("OPENAI_API_KEY")
	s.press("enter")
	s.Update(tea.PasteMsg{Content: "sk-secret-value\n"})
	if text := s.text(); strings.Contains(text, "sk-secret") || !strings.Contains(text, "•••") {
		t.Errorf("the value is on the screen:\n%s", text)
	}
	s.press("enter")
	s.called("set-token web custom OPENAI_API_KEY len=15")
	s.shows("custom: OPENAI_API_KEY")

	// Hugging Face: the page opens, and only one such token can be added.
	s.press("enter").pick("Settings").pick("Tokens").pick("+ Add Hugging Face token")
	s.called("open-url https://hf.test/tokens")
	s.press("hf_abc", "enter")
	s.called("set-token web hf HF_TOKEN len=6")
	s.press("enter").pick("Settings").pick("Tokens")
	if got, want := s.labels(), []string{"GitHub", "OPENAI_API_KEY", "HF_TOKEN", "+ Add custom token"}; !slices.Equal(got, want) {
		t.Errorf("tokens afterwards: %q", got)
	}

	s.pick("OPENAI_API_KEY").pick("Remove")
	s.shows("it stays valid at its service")
	s.press("right", "enter")
	s.called("remove-token web OPENAI_API_KEY")
}

func TestGitHubTokenFlow(t *testing.T) {
	s := start(t)
	s.onto("web").press("enter").pick("Settings").pick("Tokens").pick("GitHub")
	if got, want := s.labels(), []string{"Issue a token"}; !slices.Equal(got, want) {
		t.Errorf("without a token: %q", got)
	}
	// No preset is preselected for good: the user has to pick one.
	s.pick("Issue a token")
	if got, want := s.labels(), []string{"read", "push", "push+pr"}; !slices.Equal(got, want) {
		t.Errorf("presets: %q", got)
	}
	s.pick("push")
	s.called("github-issue web push")
	s.called("open-url https://github.test/new?preset=push")
	s.shows("token name: bulkhead-web")

	s.press("not-a-token", "enter")
	s.shows("does not look like a fine-grained token")
	s.top().(*input).value = []rune("github_pat_ABC123")
	// ctrl+o opens the page again.
	s.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	if count := strings.Count(strings.Join(s.backend.calls, "\n"), "open-url"); count != 2 {
		t.Errorf("the page was opened %d times", count)
	}
	s.press("enter")
	s.called("set-github web push bulkhead-web")
	s.shows("github: push · octocat · 2 repos · expires in 30d")

	s.press("enter").pick("Settings").pick("Tokens").pick("GitHub")
	if got := s.labels(); !slices.Equal(got[1:], []string{"---", "Replace", "Revoke"}) {
		t.Errorf("with a token: %q", got)
	}
	s.pick("Revoke").press("right", "enter")
	s.called("revoke-github web")
}

func TestDoctorShowsInTheDetailAndInTheNameColor(t *testing.T) {
	s := start(t)
	web := s.backend.find("web")
	web.Findings = []doctor.Finding{{Level: doctor.OK, Message: "dev has no capabilities"}, {Level: doctor.OK, Message: "holder is healthy"}}
	s.settle(s.reload(false))
	s.onto("web")
	s.shows("doctor: ok (2 checks)")

	// An opt-in is always shown, apart, and does not colour the name.
	web.Findings = append(web.Findings, doctor.Finding{Level: doctor.Warn, Standing: true, Message: "ssh is published to the network"})
	s.settle(s.reload(false))
	s.shows("doctor: ok (2 checks) · 1 opt-in")
	if got := envStyle(*s.env("id-web"), reachState{}); got.color != green {
		t.Errorf("with an opt-in only, the name is %+v", got)
	}

	web.Findings = append(web.Findings, doctor.Finding{Level: doctor.Fail, Message: "dev is privileged"}, doctor.Finding{Level: doctor.Warn, Message: "created by an older bh"})
	s.settle(s.reload(false))
	s.shows("doctor: 1 FAIL / 1 warn · 1 opt-in", "dev is privileged", "created by an older bh")
	if got := envStyle(*s.env("id-web"), reachState{}); got.color != red {
		t.Errorf("with a failure, the name is %+v", got)
	}

	// Stopped, the findings do not paint the name.
	web.Status = stopped()
	s.settle(s.reload(false))
	if got := envStyle(*s.env("id-web"), reachState{}); got.color != gray {
		t.Errorf("stopped, the name is %+v", got)
	}
}

// A program that fails keeps the terminal until Enter, so that what it
// printed can be read; one that ends well, or in a way that is not ours to
// judge, gives the screen back at once.
func TestHandoverWaitsAfterAFailure(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	run := func(script string, failed func(error) bool, intro string) (err error, out string, unread string) {
		in := strings.NewReader("\nrest")
		var written strings.Builder
		h := &handover{what: "the thing", command: exec.Command("sh", "-c", script), failed: failed, intro: intro}
		h.command.Stdin = strings.NewReader("") // the program itself reads nothing
		h.SetStdin(in)
		h.SetStdout(&written)
		h.SetStderr(io.Discard)
		err = h.Run()
		rest, _ := io.ReadAll(in)
		return err, written.String(), string(rest)
	}

	err, out, unread := run("echo WARNING; exit 255", nil, "")
	if err == nil || !strings.Contains(out, "WARNING") || !strings.Contains(out, "Press Enter to return") || unread != "rest" {
		t.Errorf("failure: err=%v out=%q unread=%q", err, out, unread)
	}
	err, out, unread = run("exit 0", nil, "Ctrl-C returns")
	if err != nil || out != "Ctrl-C returns\n" || unread != "\nrest" {
		t.Errorf("success: err=%v out=%q unread=%q", err, out, unread)
	}
	// The status of a shell is the shell's own business.
	err, out, unread = run("exit 3", func(error) bool { return false }, "")
	if err != nil || out != "" || unread != "\nrest" {
		t.Errorf("not ours to judge: err=%v out=%q unread=%q", err, out, unread)
	}
}

// A menu is as wide as what it says, not as wide as the screen: the values
// at the right edge of its rows must not stretch it.
func TestMenuWithValuesStaysNarrow(t *testing.T) {
	s := start(t)
	s.Update(tea.WindowSizeMsg{Width: 200, Height: 50})
	narrow := &menu{name: "Runtime", build: func(*Model) []item {
		return []item{{label: "Restart", value: "unless-stopped"}, {label: "GPU", value: "off"}, {separator: true}, {label: "Save and close"}}
	}}
	for _, line := range s.dialog(narrow) {
		if w := width(line); w != 40 {
			t.Fatalf("a dialog line is %d cells wide, want the minimum of 40: %q", w, sanitize(line))
		}
	}
	wide := &menu{name: "Keys", build: func(*Model) []item {
		return []item{{label: strings.Repeat("k", 50), value: "attached"}}
	}}
	if w := width(s.dialog(wide)[0]); w != 2+50+3+len("attached")+4 {
		t.Errorf("a menu with a long row is %d cells wide", w)
	}
}

// The key hints are the last line whether or not there is a message; the
// message takes a row from the panes, right above the hints, and gives it
// back when it goes. No row is left empty.
func TestMessageSitsAboveTheHints(t *testing.T) {
	s := start(t)
	s.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	plain := func(line string) string { return strings.TrimSpace(sanitize(line)) }

	quiet := s.screen()
	if len(quiet) != 30 || !strings.Contains(plain(quiet[29]), "q quit") || !strings.HasPrefix(plain(quiet[28]), "└") {
		t.Errorf("without a message: %d lines, last two %q / %q", len(quiet), plain(quiet[28]), plain(quiet[29]))
	}

	s.say(red, "%s", "something went wrong\nwith details")
	loud := s.screen()
	if len(loud) != 30 || !strings.Contains(plain(loud[29]), "q quit") ||
		!strings.HasPrefix(plain(loud[28]), "something went wrong") || !strings.HasPrefix(plain(loud[27]), "└") {
		t.Errorf("with a message: %d lines, last three %q / %q / %q", len(loud), plain(loud[27]), plain(loud[28]), plain(loud[29]))
	}
}

// Snapshot, Roll back and Archive work on a stopped environment; running,
// they say why not. An archived environment offers the way back and
// little else.
func TestSnapshotsAndArchive(t *testing.T) {
	s := start(t)
	s.backend.snapshots = []SnapshotEntry{
		{Name: "2026-11-13T090000Z.tar.gz", About: "2026-11-13 18:00  1.2GB  manual  before the upgrade"},
		{Name: "2026-11-12T090000Z.tar.gz", About: "2026-11-12 18:00  1.1GB  manual"},
	}

	// Running: the items are there and say what is in the way.
	s.onto("web").press("enter").pick("Maintenance").pick("Snapshot")
	s.shows(`stop "web" first`)
	if slices.Contains(s.backend.calls, "snapshot web") {
		t.Fatal("a snapshot of a running environment was taken")
	}

	s.press("esc", "esc").onto("web").press("enter").pick("Stop")
	s.onto("web").press("enter").pick("Maintenance").pick("Snapshot")
	s.called("snapshot web")
	s.shows(`snapshot 2026-11-13T090000Z of "web" taken`)

	// Rolling back names the snapshot and asks.
	s.onto("web").press("enter").pick("Maintenance").pick("Roll back")
	s.shows("before the upgrade", "2026-11-12 18:00")
	s.press("down", "enter")
	s.shows("Replace the workspace and the home directory with this snapshot?")
	if slices.ContainsFunc(s.backend.calls, func(call string) bool { return strings.HasPrefix(call, "rollback") }) {
		t.Fatal("rolled back before the answer")
	}
	s.press("right", "enter")
	s.called("rollback web 2026-11-12T090000Z.tar.gz")
	s.shows("how it was is in the snapshot 2026-11-13T100000Z")

	// Archive asks, and the environment stays in the list.
	s.onto("web").press("enter").pick("Maintenance").pick("Archive")
	s.shows("Take a snapshot, then remove the containers and volumes?")
	s.press("right", "enter")
	s.called("archive web")
	s.onto("web")
	s.shows("state: archived", "Unarchive brings the")
	s.press("enter")
	if got, want := s.labels(), []string{"Unarchive", "---", "Rename", "Destroy"}; !slices.Equal(got, want) {
		t.Errorf("archived: %q", got)
	}
	s.pick("Unarchive")
	s.called("unarchive web")
	s.onto("web").press("enter")
	if !slices.Contains(s.labels(), "Start") {
		t.Errorf("after unarchive: %q", s.labels())
	}
}
