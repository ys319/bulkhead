package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/engine/enginetest"
	"github.com/ys319/bulkhead/internal/github/githubtest"
	"github.com/ys319/bulkhead/internal/image"
)

// noBuild fails the test if a command tries to build an image.
type noBuild struct{ t *testing.T }

func (b noBuild) Build(context.Context, string, image.Source, image.Options, io.Writer) error {
	b.t.Error("a build was started")
	return errors.New("no builds in tests")
}

// bench is a bh with a temporary configuration directory and a fake engine.
type bench struct {
	t       *testing.T
	dir     string
	engine  *enginetest.Fake
	sshArgs []string
	sshExit int
	dialErr error
	stdin   string
	legacy  string          // the TypeScript version's directory
	home    string          // where ~/.ssh is
	tools   []string        // programs of the host that were run, with their arguments
	path    map[string]bool // programs in PATH
	typing  bool            // stdin is a terminal
	builder image.Builder   // nil: any build fails the test
	raw     int             // how often the terminal was put in raw mode
	web     *githubtest.Web // the services tokens come from; any other request fails the test
	opened  []string        // addresses handed to the browser
	secret  string          // what is pasted at a hidden prompt
	now     time.Time       // the clock; zero for the real one
}

func newBench(t *testing.T) *bench {
	root := t.TempDir()
	return &bench{
		t: t, dir: filepath.Join(root, "bh"), legacy: filepath.Join(root, "bulkhead"), home: filepath.Join(root, "home"),
		engine: enginetest.New(), path: map[string]bool{"code": true},
		web: githubtest.New(t),
	}
}

// run runs one bh command and returns its exit code and output.
func (b *bench) run(args ...string) (code int, stdout, stderr string) {
	b.t.Helper()
	var out, errOut bytes.Buffer
	var builder image.Builder = noBuild{b.t}
	if b.builder != nil {
		builder = b.builder
	}
	a := &app{
		stdin: strings.NewReader(b.stdin), stdout: &out, stderr: &errOut,
		getenv: func(key string) string {
			switch key {
			case "BH_CONFIG_DIR":
				return b.dir
			case "BULKHEAD_CONFIG_DIR":
				return b.legacy
			case "HOME":
				return b.home
			}
			return ""
		},
		goos: "linux",
		dial: func(func(string) string) (dockerEngine, func(), error) {
			return b.engine, func() {}, b.dialErr
		},
		ssh: func(args []string) (int, error) {
			b.sshArgs = args
			return b.sshExit, nil
		},
		builder: builder,
		rawTerminal: func() (func(), func() (uint, uint), error) {
			b.raw++
			return func() {}, func() (uint, uint) { return 40, 120 }, nil
		},
		tool: func(name string, args []string) (string, error) {
			b.tools = append(b.tools, name+" "+strings.Join(args, " "))
			return "", nil
		},
		lookPath: func(name string) (string, error) {
			if b.path[name] {
				return "/usr/bin/" + name, nil
			}
			return "", errors.New("not found")
		},
		isTerminal: func() bool { return b.typing },
		httpClient: &http.Client{Transport: b.web},
		openURL: func(address string) error {
			b.opened = append(b.opened, address)
			return nil
		},
		readSecret: func(prompt string) (string, error) {
			errOut.WriteString(prompt)
			return b.secret, nil
		},
		now: func() time.Time {
			if b.now.IsZero() {
				return time.Now()
			}
			return b.now
		},
	}
	code = a.run(context.Background(), args)
	return code, out.String(), errOut.String()
}

// ok runs a command that must succeed and returns its stdout.
func (b *bench) ok(args ...string) string {
	b.t.Helper()
	code, stdout, stderr := b.run(args...)
	if code != 0 {
		b.t.Fatalf("bh %s: exit %d\n%s%s", strings.Join(args, " "), code, stdout, stderr)
	}
	return stdout
}

// fails runs a command that must fail with the code and returns its stderr.
func (b *bench) fails(code int, args ...string) string {
	b.t.Helper()
	got, stdout, stderr := b.run(args...)
	if got != code {
		b.t.Fatalf("bh %s: exit %d, want %d\n%s%s", strings.Join(args, " "), got, code, stdout, stderr)
	}
	return stderr
}

func (b *bench) env(name string) config.Environment {
	b.t.Helper()
	cfg, err := config.Store{Dir: b.dir}.Load()
	if err != nil {
		b.t.Fatal(err)
	}
	env, err := cfg.Find(name)
	if err != nil {
		b.t.Fatal(err)
	}
	return env
}

func TestCreateWritesTheSettings(t *testing.T) {
	b := newBench(t)
	out := b.ok("create", "web")
	if !strings.Contains(out, `Created environment "web"`) || !strings.Contains(out, "127.0.0.1:22001") {
		t.Errorf("output:\n%s", out)
	}
	web := b.env("web")
	if !config.ValidID(web.ID) || len(web.ID) != 12 || web.SSHPort != 22001 || web.Restart != "unless-stopped" || web.Publish {
		t.Errorf("web = %+v", web)
	}
	if len(b.engine.Calls) != 0 {
		t.Errorf("create touched Docker: %v", b.engine.Calls)
	}

	// The next one gets the next port; flags can stand on either side.
	b.ok("create", "--port", "23000", "api", "--publish", "--restart", "no")
	api := b.env("api")
	if api.SSHPort != 23000 || !api.Publish || api.Restart != "no" {
		t.Errorf("api = %+v", api)
	}
	b.ok("create", "third")
	if port := b.env("third").SSHPort; port != 22002 {
		t.Errorf("third got port %d, want 22002", port)
	}
}

func TestCreateRefuses(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	tests := []struct {
		name string
		code int
		args []string
		want string
	}{
		{"a taken name", 1, []string{"create", "web"}, "already exists"},
		{"a taken port", 1, []string{"create", "other", "--port", "22001"}, "already the ssh port"},
		{"a bad name", 2, []string{"create", "Web_1"}, "lowercase"},
		{"a bad restart policy", 1, []string{"create", "other", "--restart", "always"}, "restart"},
		{"an unknown flag", 2, []string{"create", "other", "--gpu"}, "not defined"},
		{"no name", 2, []string{"create"}, "usage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if stderr := b.fails(tt.code, tt.args...); !strings.Contains(stderr, tt.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr, tt.want)
			}
		})
	}
	if cfg, _ := (config.Store{Dir: b.dir}).Load(); len(cfg.Envs) != 1 {
		t.Errorf("a refused create left something behind: %+v", cfg.Envs)
	}
}

func TestUpDownRestart(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	id := b.env("web").ID
	holder, dev := "bulkhead-"+id+"-holder", "bulkhead-"+id+"-dev"

	out := b.ok("up", "web")
	if !strings.Contains(out, "web is up") {
		t.Errorf("output: %s", out)
	}
	if got, want := b.engine.Lifecycle(), []string{"create " + holder, "start " + holder, "create " + dev, "start " + dev}; !slices.Equal(got, want) {
		t.Fatalf("up: %v", got)
	}
	// The resources carry this directory's id, and the hostname is the name.
	owner, _ := config.Store{Dir: b.dir}.Owner()
	if got := b.engine.Containers[dev].Labels["bulkhead.owner"]; got != owner || owner == "" {
		t.Errorf("owner label = %q, want %q", got, owner)
	}
	// The machine key was made on the way and is what gets in.
	if _, err := os.Stat(filepath.Join(b.dir, "id_ed25519")); err != nil {
		t.Errorf("no machine key: %v", err)
	}

	b.engine.Calls = nil
	b.ok("up", "web")
	if got := b.engine.Lifecycle(); len(got) != 0 {
		t.Errorf("a second up did something: %v", got)
	}

	b.ok("down", "web")
	if got, want := b.engine.Lifecycle(), []string{"stop " + dev, "stop " + holder}; !slices.Equal(got, want) {
		t.Errorf("down: %v", got)
	}
	if len(b.engine.Containers) != 2 {
		t.Error("down removed containers")
	}

	b.ok("up", "web")
	b.engine.Calls = nil
	b.ok("restart", "web")
	if got, want := b.engine.Lifecycle(), []string{
		"stop " + dev, "stop " + holder, "remove " + dev, "remove " + holder,
		"create " + holder, "start " + holder, "create " + dev, "start " + dev,
	}; !slices.Equal(got, want) {
		t.Errorf("restart: %v", got)
	}
}

// Settings that cannot be realized yet stop up before Docker is touched:
// coming up without them would look like success.
func TestUpRefusesWhatItCannotRealize(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	store := config.Store{Dir: b.dir}
	web := b.env("web")

	change := func(edit func(*config.Environment)) {
		changed := web
		edit(&changed)
		if err := store.SaveEnvironment(changed); err != nil {
			t.Fatal(err)
		}
	}
	change(func(e *config.Environment) { e.GPU = "cdi" })
	if stderr := b.fails(1, "up", "web"); !strings.Contains(stderr, "no device is chosen") {
		t.Errorf("stderr: %s", stderr)
	}
	change(func(e *config.Environment) {
		e.AuthorizedKeys = []string{"SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
	})
	if stderr := b.fails(1, "up", "web"); !strings.Contains(stderr, "not registered") {
		t.Errorf("stderr: %s", stderr)
	}
	if len(b.engine.Calls) != 0 {
		t.Errorf("Docker was touched: %v", b.engine.Calls)
	}

	if stderr := b.fails(1, "up", "nope"); !strings.Contains(stderr, `no environment named "nope"`) {
		t.Errorf("stderr: %s", stderr)
	}
}

func TestListAndStatus(t *testing.T) {
	b := newBench(t)
	if out := b.ok("list"); !strings.Contains(out, "No environments yet") {
		t.Errorf("empty list: %s", out)
	}

	b.ok("create", "web")
	b.ok("create", "api")
	store := config.Store{Dir: b.dir}
	if err := store.SaveEnvironment(config.Environment{ID: "remote000001", Name: "box", Type: config.TypeRemote, Host: "example.com", SSHPort: 22}); err != nil {
		t.Fatal(err)
	}
	b.ok("up", "web")

	rows := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(b.ok("list")), "\n")[1:] {
		fields := strings.Fields(line)
		rows[fields[0]] = fields[1:]
	}
	want := map[string][]string{
		"web": {"running", "127.0.0.1:22001"},
		"api": {"not", "created", "127.0.0.1:22002"},
		"box": {"remote", "example.com:22"},
	}
	for name, fields := range want {
		if !slices.Equal(rows[name], fields) {
			t.Errorf("list row %s = %v, want %v", name, rows[name], fields)
		}
	}

	status := b.ok("status", "web")
	for _, part := range []string{"name:     web", "state:    running", "ssh:      127.0.0.1:22001", "holder:   running (healthy)", "dev:      running"} {
		if !strings.Contains(status, part) {
			t.Errorf("status lacks %q:\n%s", part, status)
		}
	}

	// Only the holder left running: something is up, but not the whole.
	b.engine.Containers["bulkhead-"+b.env("web").ID+"-dev"].Status = "exited"
	if status := b.ok("status", "web"); !strings.Contains(status, "degraded") || !strings.Contains(status, "bh up web") {
		t.Errorf("status of a half-running environment:\n%s", status)
	}
}

// The list is about what is configured; an engine that does not answer
// makes the states unknown, not the list empty.
func TestListWithoutDocker(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.dialErr = errors.New("cannot connect")

	code, stdout, stderr := b.run("list")
	if code != 0 || !strings.Contains(stdout, "unknown") || !strings.Contains(stderr, "cannot connect") {
		t.Errorf("exit %d\n%s%s", code, stdout, stderr)
	}
}

func TestSSH(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")

	if stderr := b.fails(1, "ssh", "web"); !strings.Contains(stderr, "not running") || b.sshArgs != nil {
		t.Errorf("ssh into a stopped environment: %q, ssh ran with %v", stderr, b.sshArgs)
	}

	b.ok("up", "web")
	b.ok("ssh", "web", "uname", "-a")
	want := []string{
		"-i", filepath.Join(b.dir, "id_ed25519"), "-p", "22001",
		"-o", "StrictHostKeyChecking=accept-new", "-o", "IdentitiesOnly=yes",
		"developer@127.0.0.1", "uname", "-a",
	}
	if !slices.Equal(b.sshArgs, want) {
		t.Errorf("ssh args:\n  got  %v\n  want %v", b.sshArgs, want)
	}

	// The remote command's status is bh's status.
	b.sshExit = 7
	b.fails(7, "ssh", "web", "false")
}

func TestDestroy(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	b.ok("up", "web")
	id := b.env("web").ID
	dir := config.Store{Dir: b.dir}.EnvDir(id)

	// The wrong answer, and no answer at all, remove nothing.
	for _, answer := range []string{"yes\n", ""} {
		b.stdin = answer
		if stderr := b.fails(1, "destroy", "web"); !strings.Contains(stderr, "nothing was removed") {
			t.Errorf("stderr: %s", stderr)
		}
	}
	if _, err := os.Stat(dir); err != nil || len(b.engine.Containers) != 2 {
		t.Fatalf("an unconfirmed destroy removed something (%v, %d containers)", err, len(b.engine.Containers))
	}

	b.stdin = "web\n"
	b.engine.Calls = nil
	b.ok("destroy", "web")
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the settings are still there: %v", err)
	}
	calls := b.engine.Lifecycle()
	if len(b.engine.Containers) != 0 || !slices.Contains(calls, "rm-volume bulkhead-"+id+"_workspace") || !slices.Contains(calls, "rm-network bulkhead-"+id) {
		t.Errorf("destroy: %v", calls)
	}
}

// An environment whose settings cannot be read keeps its resources; it can
// be found and destroyed by its id.
func TestABrokenEnvironmentIsListedAndCanBeDestroyed(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	id := b.env("web").ID
	store := config.Store{Dir: b.dir}
	if err := os.WriteFile(store.EnvFile(id), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := b.run("list")
	if code != 0 || !strings.Contains(stdout, id) || !strings.Contains(stdout, "broken") || !strings.Contains(stderr, "env.json") {
		t.Errorf("list: exit %d\n%s%s", code, stdout, stderr)
	}
	if stderr := b.fails(1, "up", id); !strings.Contains(stderr, "fix the file") {
		t.Errorf("up: %s", stderr)
	}
	if out := b.ok("path", id); strings.TrimSpace(out) != store.EnvDir(id) {
		t.Errorf("path: %s", out)
	}

	b.ok("destroy", id, "--yes")
	if _, err := os.Stat(store.EnvDir(id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the directory is still there: %v", err)
	}
	if !slices.Contains(b.engine.Calls, "rm-volume bulkhead-"+id+"_home") {
		t.Errorf("destroy did not remove the volumes: %v", b.engine.Calls)
	}
}

func TestUsage(t *testing.T) {
	b := newBench(t)
	if stderr := b.fails(2); !strings.Contains(stderr, "usage: bh") {
		t.Errorf("no arguments: %s", stderr)
	}
	if stderr := b.fails(2, "frobnicate"); !strings.Contains(stderr, `unknown command "frobnicate"`) {
		t.Errorf("unknown command: %s", stderr)
	}
	if out := b.ok("help"); !strings.Contains(out, "create <name>") {
		t.Errorf("help: %s", out)
	}
	if out := b.ok("path"); strings.TrimSpace(out) != b.dir {
		t.Errorf("path: %s", out)
	}
}

// The socket opt-in works, and is said out loud wherever the environment
// is started or shown.
func TestTheDockerSocketOptInIsNeverSilent(t *testing.T) {
	b := newBench(t)
	b.ok("create", "web")
	web := b.env("web")
	web.DockerSocket = true
	if err := (config.Store{Dir: b.dir}).SaveEnvironment(web); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := b.run("up", "web")
	if code != 0 || !strings.Contains(stderr, "not isolated from the host") {
		t.Errorf("up: exit %d, stderr %q", code, stderr)
	}
	if !slices.Contains(b.engine.Calls, "probe-socket") {
		t.Errorf("the socket's group was not looked up: %v", b.engine.Calls)
	}
	if out := b.ok("list"); !strings.Contains(out, "docker socket (not isolated)") {
		t.Errorf("list:\n%s", out)
	}
	if out := b.ok("status", "web"); !strings.Contains(out, "boundary: none") {
		t.Errorf("status:\n%s", out)
	}
}
