package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/forward"
)

// forwardBench is a bench with a running local environment (web) and a
// remote one (box).
func forwardBench(t *testing.T) *bench {
	t.Helper()
	b := newBench(t)
	b.ok("create", "web")
	b.ok("up", "web")
	b.ok("remote", "add", "studio.test:22001/box")
	return b
}

// One run carries saved forwards and ports written on the spot, over the
// ssh connection of the environment — the same for a remote one.
func TestForwardRunCarriesNamedAndUnnamedPortsTogether(t *testing.T) {
	b := forwardBench(t)
	out := b.ok("forward", "save", "web", "api", "host:8000", "host:9000:5432", "--no-run")
	if !strings.Contains(out, `Saved "api" for "web": host:8000 host:9000:5432`) || !strings.Contains(out, "From now on: bh forward run web api") {
		t.Errorf("save: %s", out)
	}
	if len(b.tunnels) != 0 {
		t.Fatalf("--no-run opened something: %v", b.tunnels)
	}

	lock := config.Store{Dir: b.dir}.ForwardLock(b.env("web").ID, "api")
	held := false
	b.tunnel = func(context.Context, io.Writer) (int, error) {
		// The saved set is held for as long as the connection is there.
		_, held = forward.Holder(lock)
		return 255, nil
	}
	code, out, errOut := b.run("forward", "run", "web", "api", "8080:80", "3000")
	if code != 1 || !strings.Contains(errOut, `the forwards of "web" are closed: ssh ended with status 255`) {
		t.Errorf("run: exit %d, %s", code, errOut)
	}
	for _, line := range []string{
		`Forwarding for "web" until Ctrl-C:`,
		"  the environment's localhost:8000 → this machine's port 8000  (api)",
		"  the environment's localhost:5432 → this machine's port 9000  (api)",
		"  this machine's localhost:8080 → the environment's port 80\n",
		"  this machine's localhost:3000 → the environment's port 3000\n",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("run does not say %q:\n%s", line, out)
		}
	}
	if !held {
		t.Error("the saved set was not held while it was open")
	}
	if _, still := forward.Holder(lock); still {
		t.Error("the saved set is still held after the run")
	}
	args := strings.Join(b.tunnels[0], " ")
	for _, part := range []string{
		"-N -R 127.0.0.1:8000:localhost:8000 -R 127.0.0.1:5432:localhost:9000 -L 127.0.0.1:8080:127.0.0.1:80 -L 127.0.0.1:3000:127.0.0.1:3000",
		"-p 22001", "-o ExitOnForwardFailure=yes", "-o BatchMode=yes", "-o IdentitiesOnly=yes",
	} {
		if !strings.Contains(args, part) {
			t.Errorf("ssh is not told %q:\n%s", part, args)
		}
	}
	if !strings.HasSuffix(args, " developer@127.0.0.1") {
		t.Errorf("ssh goes to: %s", args)
	}

	// The remote environment: the same, to its address.
	b.tunnels = nil
	b.fails(1, "forward", "run", "box", "host:8000")
	if args := strings.Join(b.tunnels[0], " "); !strings.Contains(args, "-R 127.0.0.1:8000:localhost:8000") || !strings.HasSuffix(args, " developer@studio.test") {
		t.Errorf("remote: %s", args)
	}
}

// Saving goes on to open what was saved; what stands in the way of that
// is said and is no failure of the saving.
func TestForwardSaveOpensWhatItSaved(t *testing.T) {
	b := forwardBench(t)
	b.tunnel = func(context.Context, io.Writer) (int, error) { return 255, nil }
	code, out, _ := b.run("forward", "save", "web", "app", "3000")
	if code != 1 || len(b.tunnels) != 1 || !strings.Contains(out, "From now on: bh forward run web app") || !strings.Contains(out, `Forwarding for "web"`) {
		t.Errorf("save on a running environment: exit %d, %d connection(s)\n%s", code, len(b.tunnels), out)
	}
	if set, found := forward.Find(b.env("web").Forwards, "app"); !found || !set.On || set.Summary() != "3000" {
		t.Errorf("saved: %+v", b.env("web").Forwards)
	}
	// Saving again replaces, where it stands.
	b.ok("forward", "save", "web", "other", "9000", "--no-run")
	if out := b.ok("forward", "save", "web", "app", "3000", "8080:80", "--no-run"); !strings.Contains(out, `Replaced "app"`) {
		t.Errorf("replace: %s", out)
	}
	if got := b.env("web").Forwards; len(got) != 2 || got[0].Name != "app" || got[0].Summary() != "3000 8080:80" {
		t.Errorf("after replacing: %+v", got)
	}

	// Stopped: saved, and said that nothing is open.
	b.ok("down", "web")
	b.tunnels = nil
	out = b.ok("forward", "save", "web", "app", "3000")
	if len(b.tunnels) != 0 || !strings.Contains(out, `Not opened now: "web" is not running`) {
		t.Errorf("save on a stopped environment: %v\n%s", b.tunnels, out)
	}
	// Open in another bh: saved, and said who has it.
	b.ok("up", "web")
	lock, err := forward.Acquire(config.Store{Dir: b.dir}.ForwardLock(b.env("web").ID, "app"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	out = b.ok("forward", "save", "web", "app", "3001")
	if len(b.tunnels) != 0 || !strings.Contains(out, `Not opened here: "app" of "web" is open in another bh (process `) {
		t.Errorf("save while another bh has it: %v\n%s", b.tunnels, out)
	}
}

// What is open is in sight wherever environments are listed.
func TestOpenForwardsShowInTheLists(t *testing.T) {
	b := forwardBench(t)
	b.path["ssh"], b.path["docker"] = true, true
	b.ok("forward", "save", "web", "api", "host:8000", "8080:80", "--no-run")
	b.ok("forward", "save", "box", "app", "3000", "--no-run")
	b.ok("forward", "off", "box", "app")

	if out := b.ok("forward", "list"); !strings.Contains(out, "web  api   on     host:8000 8080:80") || !strings.Contains(out, "box  app   off    3000") {
		t.Errorf("list:\n%s", out)
	}
	if out := b.ok("doctor", "web"); !strings.Contains(out, `  ok   the forwards "api" let the environment reach this machine's port 8000, while they are open (they are not now)`) {
		t.Errorf("doctor:\n%s", out)
	}

	lock, err := forward.Acquire(config.Store{Dir: b.dir}.ForwardLock(b.env("web").ID, "api"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if out := b.ok("forward", "list", "web"); !strings.Contains(out, "on, open (process ") || strings.Contains(out, "box") {
		t.Errorf("list web:\n%s", out)
	}
	if out := b.ok("list"); !strings.Contains(out, "forwarding api") {
		t.Errorf("bh list:\n%s", out)
	}
	if out := b.ok("status", "web"); !strings.Contains(out, "forward:  api (on, open (process ") {
		t.Errorf("status:\n%s", out)
	}
	if out := b.ok("status", "box"); !strings.Contains(out, "forward:  app (off): 3000") {
		t.Errorf("status of the remote one:\n%s", out)
	}
	if out := b.ok("doctor", "web"); !strings.Contains(out, "port 8000, and they are open now") {
		t.Errorf("doctor while open:\n%s", out)
	}
	// One bh at a time has a saved set open.
	if err := b.fails(1, "forward", "run", "web", "api"); !strings.Contains(err, `"api" of "web" is open in another bh (process `) {
		t.Errorf("a second run: %s", err)
	}
	if len(b.tunnels) != 0 {
		t.Errorf("a second run connected: %v", b.tunnels)
	}
}

func TestForwardRunWithoutWordsOpensWhatIsOn(t *testing.T) {
	b := forwardBench(t)
	if err := b.fails(1, "forward", "run", "web"); !strings.Contains(err, "has no saved forwards that are on") {
		t.Errorf("nothing saved: %s", err)
	}
	b.ok("forward", "save", "web", "a", "3000", "--no-run")
	b.ok("forward", "save", "web", "b", "host:4000", "--no-run")
	b.ok("forward", "save", "web", "c", "5000", "--no-run")
	out := b.ok("forward", "off", "web", "b")
	if !strings.Contains(out, `"b" of "web" is off`) || b.env("web").Forwards[1].On {
		t.Errorf("off: %s", out)
	}
	b.fails(1, "forward", "run", "web")
	if args := strings.Join(b.tunnels[0], " "); !strings.Contains(args, "-L 127.0.0.1:3000:127.0.0.1:3000 -L 127.0.0.1:5000:127.0.0.1:5000") || strings.Contains(args, "4000") {
		t.Errorf("run without words: %s", args)
	}
	// By name, a set that is off opens all the same.
	b.tunnels = nil
	b.fails(1, "forward", "run", "web", "b")
	if args := strings.Join(b.tunnels[0], " "); !strings.Contains(args, "-R 127.0.0.1:4000:localhost:4000") {
		t.Errorf("run by name: %s", args)
	}
	b.ok("forward", "on", "web", "b")
	if !b.env("web").Forwards[1].On {
		t.Error("on did not switch it on")
	}
	b.ok("forward", "remove", "web", "a")
	if got := b.env("web").Forwards; len(got) != 2 || got[0].Name != "b" {
		t.Errorf("after remove: %+v", got)
	}
}

func TestForwardRefuses(t *testing.T) {
	b := forwardBench(t)
	b.ok("forward", "save", "web", "app", "3000", "--no-run")
	for _, c := range []struct {
		code int
		args []string
		want string
	}{
		{2, []string{"forward"}, "usage: bh forward"},
		{2, []string{"forward", "run"}, "usage: bh forward run"},
		{1, []string{"forward", "run", "nope", "3000"}, `no environment named "nope"`},
		{1, []string{"forward", "run", "web", "nope"}, `"web" has no saved forwards named "nope"`},
		{2, []string{"forward", "run", "web", "99999"}, "between 1 and 65535"},
		{2, []string{"forward", "run", "web", "8080>80"}, "is not a port to forward"},
		{2, []string{"forward", "run", "web", "app", "app"}, "named twice"},
		{1, []string{"forward", "run", "web", "app", "3000:81"}, "both listen on port 3000 of this machine"},
		{2, []string{"forward", "save", "web", "app"}, "usage: bh forward save"},
		{2, []string{"forward", "save", "web", "host", "3000"}, "not a name"},
		{2, []string{"forward", "save", "web", "3000", "3000"}, "starts with a letter"},
		{2, []string{"forward", "save", "web", "x", "3000", "3000:1"}, "both listen"},
		{1, []string{"forward", "on", "web", "nope"}, "no saved forwards named"},
		{1, []string{"forward", "remove", "web", "nope"}, "no saved forwards named"},
		{2, []string{"forward", "list", "web", "box"}, "usage: bh forward list"},
	} {
		if err := b.fails(c.code, c.args...); !strings.Contains(err, c.want) {
			t.Errorf("bh %s: %s", strings.Join(c.args, " "), err)
		}
	}
	if len(b.tunnels) != 0 {
		t.Errorf("a refused run connected: %v", b.tunnels)
	}
	// A local environment has to run; nothing is tried when it does not.
	b.ok("down", "web")
	if err := b.fails(1, "forward", "run", "web", "3000"); !strings.Contains(err, `"web" is not running`) {
		t.Errorf("stopped: %s", err)
	}
	if out := b.ok("forward", "--help"); !strings.Contains(out, "host:8000") {
		t.Errorf("help: %s", out)
	}
}

// A copy of an environment does not want the same ports of this machine.
func TestSavedForwardsStayBehindWhenAnEnvironmentIsCopied(t *testing.T) {
	b := forwardBench(t)
	b.ok("forward", "save", "web", "app", "3000", "--no-run")
	b.ok("create", "copy", "--from", "web")
	if got := b.env("copy").Forwards; len(got) != 0 {
		t.Errorf("the copy has forwards: %+v", got)
	}
	if len(b.env("web").Forwards) != 1 {
		t.Error("the original lost its forwards")
	}
}

// eventually waits for what the forwarder does on its own.
func eventually(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if done() {
			return
		}
	}
	t.Fatalf("never happened: %s", what)
}

// The full-screen interface carries the saved forwards that are on: one
// connection per set, under the set's lock, tried again when it breaks —
// and when another bh has a set, it waits and takes over.
func TestTheForwarderCarriesWaitsAndTakesOver(t *testing.T) {
	store := config.Store{Dir: t.TempDir()}
	env := config.NewRemote("bbbbbbbbbbbb", "box", "studio.test", 22001)
	set := forward.Set{Name: "api", On: true, Ports: []forward.Port{{Host: 8000, Env: 8000, FromHost: true}}}

	var mu sync.Mutex
	var connections [][]string
	result := make(chan string) // what the next connection says before it ends
	a := &app{tunnel: func(ctx context.Context, args []string, output io.Writer) (int, error) {
		mu.Lock()
		connections = append(connections, args)
		mu.Unlock()
		select {
		case <-ctx.Done():
			return 0, nil
		case said := <-result:
			io.WriteString(output, "debug1: something\n"+said+"\n")
			return 255, nil
		}
	}}
	opened := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(connections)
	}
	f := newForwarder(context.Background(), a, store)
	retry := make(chan struct{})
	f.pause = func(ctx context.Context, d time.Duration) {
		if d == forwardSettle {
			return // a connection that is still there counts as open at once
		}
		select {
		case <-ctx.Done():
		case <-retry:
		}
	}
	defer f.close()
	state := func() string {
		state, detail := f.state(env.ID, set.Name)
		return state + " " + detail
	}

	// Another bh has the set: nothing is opened, and it is said who has it.
	path := store.ForwardLock(env.ID, set.Name)
	other, err := forward.Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	f.sync([]forwardWant{{env, set}})
	eventually(t, "waiting for the other bh", func() bool { return strings.HasPrefix(state(), forwardElsewhere+" another bh (process ") })
	if opened() != 0 {
		t.Fatal("connected while another bh had the set")
	}
	// The other ends: this one takes over at its next look.
	other.Release()
	retry <- struct{}{}
	eventually(t, "taking over", func() bool { return state() == forwardOpen+" " })
	if args := strings.Join(connections[0], " "); !strings.Contains(args, "-N -R 127.0.0.1:8000:localhost:8000") || !strings.HasSuffix(args, "developer@studio.test") {
		t.Errorf("the connection: %s", args)
	}
	if _, held := forward.Holder(path); !held {
		t.Error("the set is not held while this bh carries it")
	}
	// The same wish again changes nothing.
	f.sync([]forwardWant{{env, set}})
	if opened() != 1 {
		t.Errorf("%d connections after the same wish", opened())
	}

	// The connection breaks: the reason is what ssh said last, the lock is
	// free meanwhile, and it is tried again.
	result <- "bind [127.0.0.1]:8000: Address already in use"
	eventually(t, "the failure", func() bool { return state() == forwardFailed+" bind [127.0.0.1]:8000: Address already in use" })
	if _, held := forward.Holder(path); held {
		t.Error("the set is held while nothing carries it")
	}
	retry <- struct{}{}
	eventually(t, "the next try", func() bool { return opened() == 2 && state() == forwardOpen+" " })

	// Other ports under the same name: a new connection.
	changed := set
	changed.Ports = []forward.Port{{Host: 8080, Env: 80}}
	f.sync([]forwardWant{{env, changed}})
	eventually(t, "the changed set", func() bool { return opened() == 3 })
	if args := strings.Join(connections[2], " "); !strings.Contains(args, "-L 127.0.0.1:8080:127.0.0.1:80") {
		t.Errorf("the changed connection: %s", args)
	}

	// No longer wanted: the connection ends and the lock is free.
	f.sync(nil)
	eventually(t, "letting go", func() bool {
		_, held := forward.Holder(path)
		return !held
	})
	if got := state(); got != " " {
		t.Errorf("state of a set that is not carried: %q", got)
	}
	// Each change was reported.
	select {
	case <-f.changed:
	default:
		t.Error("nothing was reported")
	}
}

// The forwards on disk in a line: what tells a bh that another one changed
// them.
func TestForwardsSeenChangesWithWhatIsSaved(t *testing.T) {
	b := forwardBench(t)
	seen := func() string {
		cfg, err := config.Store{Dir: b.dir}.Load()
		if err != nil {
			t.Fatal(err)
		}
		return forwardsSeen(cfg)
	}
	var all []string
	note := func() {
		now := seen()
		if slices.Contains(all, now) {
			t.Errorf("no change seen: %q", now)
		}
		all = append(all, now)
	}
	note()
	b.ok("forward", "save", "web", "app", "3000", "--no-run")
	note()
	b.ok("forward", "off", "web", "app")
	note()
	b.ok("forward", "save", "web", "app", "3001", "--no-run")
	note()
	// Where the environment is reached is part of what a connection is
	// made from.
	b.ok("forward", "save", "box", "app", "3000", "--no-run")
	note()
	b.ok("remote", "configure", "box", "--port", "22002")
	note()
	if _, err := os.Stat(filepath.Join(b.dir, "run")); err == nil {
		t.Error("looking wrote lock files")
	}
}
