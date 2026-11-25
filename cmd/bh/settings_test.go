package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/plan"
	"github.com/ys319/bulkhead/internal/sshkey"
	"github.com/ys319/bulkhead/internal/tui"
)

// publicKey makes a real public key line with the comment.
func publicKey(t *testing.T, comment string) (line, fingerprint string) {
	t.Helper()
	line, err := sshkey.EnsureMachineKey(filepath.Join(t.TempDir(), "key"), comment)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err = sshkey.Fingerprint(line)
	if err != nil {
		t.Fatal(err)
	}
	return line, fingerprint
}

func (b *bench) sshFragment() string {
	data, err := os.ReadFile(filepath.Join(b.home, ".ssh", "bh", "config"))
	if err != nil {
		return ""
	}
	return string(data)
}

func TestKeyRegistryRoundTrip(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.ok("create", "api")
	laptop, laptopPrint := publicKey(t, "me@laptop")
	phone, phonePrint := publicKey(t, "")

	// Piped in, the label comes from the comment; given, it wins.
	b.stdin = laptop + "\n"
	if out := b.ok("key", "add"); !strings.Contains(out, laptopPrint+"  me@laptop") {
		t.Errorf("add:\n%s", out)
	}
	b.stdin = ""
	b.ok(append([]string{"key", "add", "--label", "phone"}, strings.Fields(phone)...)...)
	if err := b.fails(1, append([]string{"key", "add"}, strings.Fields(phone)...)...); !strings.Contains(err, `already registered as "phone"`) {
		t.Errorf("second add: %s", err)
	}

	b.ok("key", "attach", "web", "phone")
	b.ok("key", "attach", "web", strings.TrimPrefix(laptopPrint, "SHA256:")[:10])
	b.ok("key", "attach", "api", "phone")
	if got := b.env("web").AuthorizedKeys; !slices.Equal(got, []string{phonePrint, laptopPrint}) {
		t.Errorf("web keys = %v", got)
	}
	if err := b.fails(1, "key", "attach", "web", "phone"); !strings.Contains(err, "already attached") {
		t.Errorf("attach twice: %s", err)
	}

	list := b.ok("key", "list")
	if !strings.Contains(list, phonePrint+"  phone  → api, web") || !strings.Contains(list, "me@laptop  → web") {
		t.Errorf("list:\n%s", list)
	}
	if out := b.ok("key", "list", "web"); !strings.Contains(out, phonePrint+"  phone") {
		t.Errorf("list web:\n%s", out)
	}

	b.ok("key", "rename", "phone", "pixel")
	b.ok("key", "detach", "web", "pixel")
	if got := b.env("web").AuthorizedKeys; !slices.Equal(got, []string{laptopPrint}) {
		t.Errorf("web keys after detach = %v", got)
	}

	// Removing an attached key says where it is attached, and with --yes
	// detaches it first: an environment must never refer to a missing key.
	if err := b.fails(1, "key", "remove", "pixel"); !strings.Contains(err, "attached to: api") {
		t.Errorf("remove: %s", err)
	}
	if out := b.ok("key", "remove", "pixel", "--yes"); !strings.Contains(out, "Detached from: api") {
		t.Errorf("remove --yes:\n%s", out)
	}
	if got := b.env("api").AuthorizedKeys; len(got) != 0 {
		t.Errorf("api keys = %v", got)
	}
	if out := b.ok("key", "list"); strings.Contains(out, phonePrint) {
		t.Errorf("the removed key is still listed:\n%s", out)
	}
	if len(b.engine.Calls) != 0 {
		t.Errorf("key commands touched Docker: %v", b.engine.Calls)
	}
}

func TestKeyAddRefusesWhatIsNotOnePublicKey(t *testing.T) {
	b := newBench(t)
	line, _ := publicKey(t, "me")
	for name, input := range map[string]string{
		"a private key":  "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----\n",
		"two keys":       line + "\n" + line + "\n",
		"not a key":      "hello world\n",
		"escape in key":  strings.Replace(line, " me", " \x1b[31mme", 1) + "\n",
		"key in comment": line + " " + line + "\n",
	} {
		b.stdin = input
		if code, _, _ := b.run("key", "add"); code == 0 {
			t.Errorf("%s was registered", name)
		}
	}
	keys, err := config.Store{Dir: b.dir}.LoadKeys()
	if err != nil || len(keys) != 0 {
		t.Errorf("registry = %v, %v", keys, err)
	}

	// At a terminal with nothing given, say how instead of waiting for input.
	b.typing, b.stdin = true, ""
	if err := b.fails(2, "key", "add"); !strings.Contains(err, "pipe it in") {
		t.Errorf("at a terminal: %s", err)
	}
}

func TestKeyShowMakesTheMachineKeyOnce(t *testing.T) {
	b := newBench(t)
	first := b.ok("key", "show")
	if !strings.HasPrefix(first, "ssh-ed25519 ") || first != b.ok("key", "show") {
		t.Errorf("key show:\n%s", first)
	}
}

func TestConfigureChangesAStoppedEnvironmentOnly(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.ok("create", "api")

	b.ok("configure", "web", "--publish", "on", "--port", "23000", "--restart", "no")
	web := b.env("web")
	if !web.Publish || web.SSHPort != 23000 || web.Restart != "no" {
		t.Errorf("web = %+v", web)
	}
	if !strings.Contains(b.sshFragment(), "Port 23000") {
		t.Errorf("the ssh entry did not follow the port:\n%s", b.sshFragment())
	}

	// Refused: a port another environment has, values out of range, nothing.
	if err := b.fails(1, "configure", "web", "--port", "22002"); !strings.Contains(err, `already the ssh port of "api"`) {
		t.Errorf("port clash: %s", err)
	}
	b.fails(1, "configure", "web", "--restart", "always")
	b.fails(2, "configure", "web", "--publish", "yes")
	b.fails(2, "configure", "web")
	if err := b.fails(1, "configure", "web", "--gpu", "cdi"); !strings.Contains(err, "at least one device") {
		t.Errorf("cdi without devices: %s", err)
	}
	b.ok("configure", "web", "--gpu", "cdi", "--cdi-devices", "nvidia.com/gpu=0, nvidia.com/gpu=1")
	if got := b.env("web").CDIDevices; !slices.Equal(got, []string{"nvidia.com/gpu=0", "nvidia.com/gpu=1"}) {
		t.Errorf("cdi devices = %v", got)
	}
	b.ok("configure", "web", "--gpu", "off")

	// Running: what the containers were created from is locked.
	b.ok("up", "web")
	if err := b.fails(1, "configure", "web", "--publish", "off"); !strings.Contains(err, "is running — stop it first to change publish") {
		t.Errorf("while running: %s", err)
	}
	if !b.env("web").Publish {
		t.Error("the setting changed although the command failed")
	}
	b.ok("down", "web")
	b.ok("configure", "web", "--publish", "off")

	// The opt-in is said out loud when it is made.
	_, _, stderr := b.run("configure", "web", "--docker-socket", "on")
	if !b.env("web").DockerSocket || !strings.Contains(stderr, "not isolated") {
		t.Errorf("docker socket: %+v\n%s", b.env("web"), stderr)
	}
}

func TestRenameKeepsTheIDAndMovesTheSSHEntry(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.ok("create", "api")
	id := b.env("web").ID

	b.ok("rename", "web", "site")
	if got := b.env("site"); got.ID != id {
		t.Errorf("the id changed: %s → %s", id, got.ID)
	}
	fragment := b.sshFragment()
	if !strings.Contains(fragment, "Host bulkhead-site") || strings.Contains(fragment, "Host bulkhead-web") {
		t.Errorf("ssh entries:\n%s", fragment)
	}
	if err := b.fails(1, "rename", "site", "api"); !strings.Contains(err, "already exists") {
		t.Errorf("taken name: %s", err)
	}
	b.fails(2, "rename", "site", "Not_Valid")
	b.fails(1, "rename", "nope", "x")
}

func TestCreateFromCopiesSettingsNotData(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web", "--restart", "no")
	line, fingerprint := publicKey(t, "me")
	b.ok(append([]string{"key", "add"}, strings.Fields(line)...)...)
	b.ok("key", "attach", "web", "me")
	web := b.env("web")
	store := config.Store{Dir: b.dir}
	if err := os.WriteFile(store.DevDockerfile(web.ID), []byte("FROM bulkhead-dev-base\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	b.ok("create", "copy", "--from", "web")
	copied := b.env("copy")
	if copied.ID == web.ID || copied.SSHPort == web.SSHPort || copied.Restart != "no" || !slices.Equal(copied.AuthorizedKeys, []string{fingerprint}) {
		t.Errorf("copy = %+v", copied)
	}
	if data, _ := os.ReadFile(store.DevDockerfile(copied.ID)); string(data) != "FROM bulkhead-dev-base\n" {
		t.Errorf("dev.Dockerfile was not copied: %q", data)
	}
	if len(b.engine.Calls) != 0 {
		t.Errorf("create --from touched Docker: %v", b.engine.Calls)
	}
	b.fails(1, "create", "x", "--from", "nope")
}

func TestRemoteEnvironments(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	out := b.ok("remote", "add", "studio.test:22001/main")
	if !strings.Contains(out, `"main" → studio.test:22001`) {
		t.Errorf("add:\n%s", out)
	}
	main := b.env("main")
	if main.Local() || main.Host != "studio.test" || main.SSHPort != 22001 {
		t.Errorf("main = %+v", main)
	}
	// The same port as a local environment is fine: it is on another machine.
	if !strings.Contains(b.sshFragment(), "HostName studio.test") {
		t.Errorf("ssh entries:\n%s", b.sshFragment())
	}

	b.ok("remote", "add", "studio.test:22002/web", "--name", "studio-web")
	if err := b.fails(1, "remote", "add", "studio.test:22002/web"); !strings.Contains(err, "already exists") {
		t.Errorf("taken name: %s", err)
	}
	b.fails(2, "remote", "add", "studio.test/web")

	b.ok("remote", "configure", "main", "--host", "10.0.0.5", "--port", "22010")
	if got := b.env("main"); got.Host != "10.0.0.5" || got.SSHPort != 22010 {
		t.Errorf("main = %+v", got)
	}
	b.fails(2, "remote", "configure", "main")
	if err := b.fails(1, "remote", "configure", "web", "--port", "1"); !strings.Contains(err, "local environment") {
		t.Errorf("configure a local one: %s", err)
	}
	if err := b.fails(1, "configure", "main", "--port", "1"); !strings.Contains(err, "bh remote configure") {
		t.Errorf("configure a remote one: %s", err)
	}

	// ssh goes to the host without asking Docker.
	b.ok("ssh", "main", "true")
	if !slices.Contains(b.sshArgs, "developer@10.0.0.5") || len(b.engine.Calls) != 0 {
		t.Errorf("ssh args = %v, calls = %v", b.sshArgs, b.engine.Calls)
	}

	b.ok("remote", "remove", "main")
	if strings.Contains(b.sshFragment(), "bulkhead-main") {
		t.Errorf("the ssh entry stayed:\n%s", b.sshFragment())
	}
	b.fails(1, "remote", "remove", "web")
}

func TestOpenStartsVSCodeThroughTheSSHEntry(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	if err := b.fails(1, "open", "web"); !strings.Contains(err, "not running") {
		t.Errorf("stopped: %s", err)
	}
	b.ok("up", "web")
	b.ok("open", "web")
	if !slices.Contains(b.tools, "code --remote ssh-remote+bulkhead-web /home/developer/workspace") {
		t.Errorf("tools = %v", b.tools)
	}
	if !strings.Contains(b.sshFragment(), "Host bulkhead-web") {
		t.Errorf("ssh entries:\n%s", b.sshFragment())
	}

	b.path = map[string]bool{}
	if err := b.fails(1, "open", "web"); !strings.Contains(err, "code command was not found") {
		t.Errorf("without code: %s", err)
	}
}

// The link names the ssh entry and nothing else: no user, no port, no key.
// Those come from the entry, and the desktop asks before it connects.
func TestOpenInClaudeHandsOverALinkToTheSSHEntry(t *testing.T) {
	const link = "claude://code/new?ssh_host=bulkhead-web&ssh_folder=/home/developer/workspace"
	b := newBench(t)
	b.path = map[string]bool{} // VS Code is not needed for this
	b.ok("create", "web")
	if err := b.fails(1, "open", "web", "--in", "claude"); !strings.Contains(err, "not running") {
		t.Errorf("stopped: %s", err)
	}
	b.ok("up", "web")

	out := b.ok("open", "--in", "claude", "web")
	if !slices.Equal(b.opened, []string{link}) || len(b.tools) != 0 {
		t.Errorf("opened = %v, tools = %v", b.opened, b.tools)
	}
	if !strings.Contains(out, "bulkhead-web") || !strings.Contains(out, "confirm the connection there") {
		t.Errorf("output: %s", out)
	}
	if !strings.Contains(b.sshFragment(), "Host bulkhead-web") {
		t.Errorf("ssh entries:\n%s", b.sshFragment())
	}

	// On macOS open is waited for: it says when nothing takes the link.
	b.goos, b.opened = "darwin", nil
	b.ok("open", "web", "--in", "claude")
	if !slices.Equal(b.tools, []string{"open " + link}) || len(b.opened) != 0 {
		t.Errorf("on macOS: tools = %v, opened = %v", b.tools, b.opened)
	}

	if err := b.fails(2, "open", "web", "--in", "emacs"); !strings.Contains(err, "neither vscode nor claude") {
		t.Errorf("another program: %s", err)
	}
	if err := b.fails(2, "open", "--in", "claude"); !strings.Contains(err, "usage: bh open <env> [--in vscode|claude]") {
		t.Errorf("no environment: %s", err)
	}
}

func TestClaudeLinkEscapesWhatItCarries(t *testing.T) {
	if got, want := claudeLink("dev@build.example.com", "~/my project"), "claude://code/new?ssh_host=dev%40build.example.com&ssh_folder=~/my%20project"; got != want {
		t.Errorf("link = %s, want %s", got, want)
	}
}

// The full-screen interface offers Claude Desktop where it can be found.
func TestClaudeDesktopIsLookedForOnMacOS(t *testing.T) {
	b := newBench(t)
	a := &app{goos: "linux", getenv: func(string) string { return b.home }}
	if a.hasClaude() {
		t.Error("offered on Linux")
	}
	// Where Homebrew was told to put its applications, a folder down.
	if err := os.MkdirAll(filepath.Join(b.home, "Applications", "Homebrew", "Claude.app"), 0o755); err != nil {
		t.Fatal(err)
	}
	a = &app{goos: "darwin", getenv: func(string) string { return b.home }}
	if !a.hasClaude() {
		t.Error("not found in a folder of ~/Applications")
	}
}

func TestApplicationsAreLookedForInFoldersButNotInBundles(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{
		"Editor.app/Contents/Helpers/Claude.app", // part of another application
		"a/b/c/d/Claude.app",                     // further down than anybody files applications
		".hidden/Claude.app",
		"Tools/Linked",
		"../Elsewhere/Claude.app",
	} {
		if err := os.MkdirAll(filepath.Join(root, "Applications", dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	apps := filepath.Join(root, "Applications")
	if err := os.WriteFile(filepath.Join(apps, "Claude.app.zip"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	folders := []string{filepath.Join(root, "missing"), apps}
	if appInstalled(folders, "Claude.app", 3) {
		t.Error("found where no application is installed")
	}
	// A link to the application, in a folder: as the package managers leave it.
	if err := os.Symlink(filepath.Join(root, "Elsewhere", "Claude.app"), filepath.Join(apps, "Tools", "Linked", "Claude.app")); err != nil {
		t.Fatal(err)
	}
	if !appInstalled(folders, "Claude.app", 3) {
		t.Error("not found two folders down, through a link")
	}
	if appInstalled(folders, "Claude.app", 1) {
		t.Error("found further down than asked for")
	}
}

func TestDownAllStopsWhatRuns(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.ok("create", "api")
	b.ok("create", "idle")
	b.ok("remote", "add", "studio.test:22001/main")
	b.ok("up", "web")
	b.ok("up", "api")

	out := b.ok("down", "--all")
	if !strings.Contains(out, "api is stopped") || !strings.Contains(out, "web is stopped") || strings.Contains(out, "idle") {
		t.Errorf("down --all:\n%s", out)
	}
	for name, c := range b.engine.Containers {
		if c.Running() {
			t.Errorf("%s still runs", name)
		}
	}
	if out := b.ok("down", "--all"); !strings.Contains(out, "Nothing was running") {
		t.Errorf("second down --all:\n%s", out)
	}
}

func TestDestroyForgetsTheHostKeyAndTheSSHEntry(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web", "--port", "23001")
	b.ok("up", "web")
	knownHosts := filepath.Join(b.home, ".ssh", "known_hosts")
	if err := os.WriteFile(knownHosts, []byte("[127.0.0.1]:23001 ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := b.env("web").ID
	b.ok("destroy", "web", "--yes")
	for _, entry := range []string{"bh-" + id, "[127.0.0.1]:23001"} {
		if !slices.Contains(b.tools, "ssh-keygen -R "+entry+" -f "+knownHosts) {
			t.Errorf("%s was not forgotten: tools = %v", entry, b.tools)
		}
	}
	if strings.Contains(b.sshFragment(), "bulkhead-web") {
		t.Errorf("the ssh entry stayed:\n%s", b.sshFragment())
	}
}

// The commands ask for nothing: without the environment a command needs,
// it says how it is used — at a terminal as anywhere else. Asking is the
// full-screen interface's part.
func TestACommandWithoutAnEnvironmentSaysHowItIsUsed(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	for _, typing := range []bool{false, true} {
		b.typing = typing
		for _, command := range []string{"up", "down", "restart", "logs", "status", "ssh", "open"} {
			if err := b.fails(2, command); !strings.Contains(err, "usage: bh "+command) {
				t.Errorf("bh %s (at a terminal: %v): %s", command, typing, err)
			}
		}
	}
	if len(b.engine.Calls) != 0 {
		t.Errorf("something was done: %v", b.engine.Calls)
	}
}

// Ctrl-C cancels the context of a command. The interface must not go with
// it: the interrupt is for the program it handed the terminal to (bh logs).
func TestInterfaceOutlivesAnInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alive := false
	dir := t.TempDir()
	a := &app{
		isTerminal: func() bool { return true },
		getenv:     func(key string) string { return map[string]string{"BH_CONFIG_DIR": dir}[key] },
		goos:       "linux",
		fullScreen: func(ctx context.Context, _ tui.Backend) error {
			alive = ctx.Err() == nil
			return nil
		},
	}
	if err := a.interactive(ctx); err != nil {
		t.Fatal(err)
	}
	if !alive {
		t.Error("the interface got a context that an interrupt ends")
	}
}

// The size of /dev/shm is a setting of the machine: every environment's
// dev container is created with it, and the holder is not.
func TestShmSizeIsAMachineSettingThatReachesDev(t *testing.T) {
	b := newBench(t)
	if out := b.ok("settings"); out != "shm-size  8g\n" {
		t.Errorf("settings before any was set: %q", out)
	}
	if _, err := os.Stat(filepath.Join(b.dir, "settings.json")); err == nil {
		t.Error("printing the settings wrote the file")
	}

	b.ok("create", "web")
	b.ok("create", "api")
	b.ok("up", "web")
	web, api := b.env("web").ID, b.env("api").ID
	if got := b.engine.Created[plan.DevName(web)].Host.ShmSize; got != 8<<30 {
		t.Errorf("dev's /dev/shm without a setting = %d", got)
	}
	if got := b.engine.Created[plan.HolderName(web)].Host.ShmSize; got != 0 {
		t.Errorf("the holder's /dev/shm = %d", got)
	}

	out := b.ok("settings", "--shm-size", "16G")
	if !strings.Contains(out, "/dev/shm holds up to 16g") || !strings.Contains(out, "bh restart <env>") {
		t.Errorf("output: %s", out)
	}
	if out := b.ok("settings"); out != "shm-size  16g\n" {
		t.Errorf("settings: %q", out)
	}
	data, _ := os.ReadFile(filepath.Join(b.dir, "settings.json"))
	if !strings.Contains(string(data), `"shmSize": "16g"`) {
		t.Errorf("settings.json:\n%s", data)
	}

	// The container that exists has what it was created with, and doctor
	// says so; an environment started from now on has the new size.
	if got := b.engine.Created[plan.DevName(web)].Host.ShmSize; got != 8<<30 {
		t.Errorf("a running dev changed: %d", got)
	}
	b.path["ssh"], b.path["docker"] = true, true
	doctor := b.ok("doctor")
	for _, part := range []string{
		"  ok   an environment's /dev/shm holds up to 16g (bh settings)",
		" warn  dev was created from other settings than the current ones — the next bh up web recreates it",
	} {
		if !strings.Contains(doctor, part) {
			t.Errorf("doctor does not say %q:\n%s", part, doctor)
		}
	}
	b.ok("up", "api")
	if got := b.engine.Created[plan.DevName(api)].Host.ShmSize; got != 16<<30 {
		t.Errorf("api's /dev/shm = %d", got)
	}
	// Only dev is replaced: the holder is not made from this setting.
	b.engine.Calls = nil
	b.ok("restart", "web")
	if got := b.engine.Created[plan.DevName(web)].Host.ShmSize; got != 16<<30 {
		t.Errorf("web's /dev/shm after a restart = %d", got)
	}
	if doctor := b.ok("doctor", "web"); strings.Contains(doctor, " warn ") {
		t.Errorf("doctor after the restart:\n%s", doctor)
	}
}

func TestSettingsRefuses(t *testing.T) {
	b := newBench(t)
	for _, size := range []string{"lots", "8", "8k", "32m", ""} {
		if err := b.fails(2, "settings", "--shm-size", size); !strings.Contains(err, "--shm-size") {
			t.Errorf("--shm-size %q: %s", size, err)
		}
	}
	b.fails(2, "settings", "web")
	if _, err := os.Stat(filepath.Join(b.dir, "settings.json")); err == nil {
		t.Error("a refused size wrote the file")
	}
	if out := b.ok("settings", "--help"); !strings.Contains(out, "usage: bh settings") {
		t.Errorf("help: %s", out)
	}
}

// Settings that cannot be read stop an environment from starting: a dev
// created with a guess would not be what the user set.
func TestUnreadableSettingsStopUpAndAreNotOverwritten(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	file := filepath.Join(b.dir, "settings.json")
	broken := `{"version": 1, "shmSize": "plenty"}`
	b.write(file, broken)

	if err := b.fails(1, "up", "web"); !strings.Contains(err, file) || !strings.Contains(err, "is not a size") {
		t.Errorf("up: %s", err)
	}
	if got := b.engine.Lifecycle(); len(got) != 0 {
		t.Errorf("up touched Docker: %v", got)
	}
	if err := b.fails(1, "settings"); !strings.Contains(err, file) {
		t.Errorf("settings: %s", err)
	}
	// Setting does not paper over a file somebody edited.
	b.fails(1, "settings", "--shm-size", "16g")
	if data, _ := os.ReadFile(file); string(data) != broken {
		t.Errorf("settings.json was rewritten:\n%s", data)
	}
	b.path["ssh"], b.path["docker"] = true, true
	code, doctor, _ := b.run("doctor")
	if code != 1 || !strings.Contains(doctor, " FAIL  this machine's settings cannot be read") {
		t.Errorf("doctor (exit %d):\n%s", code, doctor)
	}
}
