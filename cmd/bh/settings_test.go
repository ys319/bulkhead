package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ys319/bulkhead/internal/config"
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

func TestACommandWithoutAnEnvironmentAsksWhich(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.ok("create", "api")
	b.ok("remote", "add", "studio.test:22001/main")

	// Not at a terminal: the usage, as before.
	b.fails(2, "up")

	b.typing, b.picks = true, "web"
	b.ok("up")
	if b.asked != "bh up — which environment?" || !slices.Equal(b.offered, []string{"api", "web"}) {
		t.Errorf("asked %q, offered %v (up is for local environments)", b.asked, b.offered)
	}
	if !b.engine.Containers["bulkhead-"+b.env("web").ID+"-dev"].Running() {
		t.Error("web was not started")
	}

	b.offered = nil
	b.ok("status")
	if !slices.Equal(b.offered, []string{"api", "main", "web"}) {
		t.Errorf("status offered %v", b.offered)
	}

	// Leaving the list does nothing and says nothing.
	b.picks = ""
	if code, _, stderr := b.run("down"); code != 1 || stderr != "" {
		t.Errorf("cancelled: exit %d, stderr %q", code, stderr)
	}
}

// Ctrl-C cancels the context of a command. The interface must not go with
// it: the interrupt is for the program it handed the terminal to (bh logs).
func TestInterfaceOutlivesAnInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alive := false
	a := &app{
		isTerminal: func() bool { return true },
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
