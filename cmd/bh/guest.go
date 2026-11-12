package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sync"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/engine"
	"github.com/ys319/bulkhead/internal/ops"
)

// Commands that reach into the containers of a local environment: their
// output, and programs run inside dev through the engine. The engine is the
// way in, so nothing here can be reached from inside an environment.

// prefixWriter writes whole lines to out, each with a prefix, under a lock
// shared with the other writers to the same out.
type prefixWriter struct {
	out     io.Writer
	lock    *sync.Mutex
	prefix  string
	pending []byte
}

func (w *prefixWriter) Write(data []byte) (int, error) {
	w.pending = append(w.pending, data...)
	for {
		end := bytes.IndexByte(w.pending, '\n')
		if end < 0 {
			return len(data), nil
		}
		w.lock.Lock()
		_, err := fmt.Fprintf(w.out, "%s%s\n", w.prefix, bytes.TrimSuffix(w.pending[:end], []byte("\r")))
		w.lock.Unlock()
		w.pending = w.pending[end+1:]
		if err != nil {
			return len(data), err
		}
	}
}

// flush writes a last line that had no line end.
func (w *prefixWriter) flush() {
	if len(w.pending) > 0 {
		w.lock.Lock()
		fmt.Fprintf(w.out, "%s%s\n", w.prefix, w.pending)
		w.lock.Unlock()
		w.pending = nil
	}
}

// streamLogs copies the output of both containers to out, line by line,
// each line marked with the container it came from.
func streamLogs(ctx context.Context, eng dockerEngine, status ops.Status, opts engine.LogOptions, out io.Writer) error {
	var lock sync.Mutex
	var wait sync.WaitGroup
	errs := make([]error, 2)
	for i, source := range []struct {
		found  *engine.Container
		prefix string
	}{{status.Holder, "holder | "}, {status.Dev, "dev    | "}} {
		if source.found == nil {
			continue
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			writer := &prefixWriter{out: out, lock: &lock, prefix: source.prefix}
			errs[i] = eng.Logs(ctx, source.found.ID, opts, writer)
			writer.flush()
		}()
	}
	wait.Wait()
	return errors.Join(errs...)
}

func (a *app) logs(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("logs", flag.ContinueOnError)
	tail := flags.Int("tail", 0, "")
	noFollow := flags.Bool("no-follow", false, "")
	timestamps := flags.Bool("timestamps", false, "")
	rest, err := parseFlags(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 || *tail < 0 {
		return usagef("usage: bh logs <env> [--tail N] [--no-follow] [--timestamps]")
	}
	_, env, err := a.localEnv("logs", rest[0])
	if err != nil {
		return err
	}
	eng, disconnect, err := a.dial(a.getenv)
	if err != nil {
		return err
	}
	defer disconnect()
	status, err := (ops.Runner{Engine: eng}).Inspect(ctx, env.ID)
	if err != nil {
		return err
	}
	if status.Holder == nil && status.Dev == nil {
		return fmt.Errorf("%q has no containers yet (start it with bh up %s)", env.Name, env.Name)
	}
	// Following ends with Ctrl-C, which is not a failure.
	return streamLogs(ctx, eng, status, engine.LogOptions{Follow: !*noFollow, Tail: *tail, Timestamps: *timestamps}, a.stdout)
}

// runningDev finds the dev container of a local environment and insists
// that it runs.
func runningDev(ctx context.Context, eng dockerEngine, env config.Environment) (*engine.Container, error) {
	status, err := (ops.Runner{Engine: eng}).Inspect(ctx, env.ID)
	if err != nil {
		return nil, err
	}
	if status.Dev == nil || !status.Dev.Running() {
		return nil, fmt.Errorf("%q is not running (start it with bh up %s)", env.Name, env.Name)
	}
	return status.Dev, nil
}

// guestBin is bulkhead's helper inside the dev image: everything bh does in
// there goes through it.
const guestBin = "/usr/local/bin/bh-guest"

// staleGuest: exit statuses that mean the image is older than this bh —
// bh-guest does not know the command (64), cannot be run (126) or is
// missing (127).
func staleGuest(code int) bool { return code == 64 || code == 126 || code == 127 }

// runInit runs init.sh in the running environment now. At the first boot of
// a fresh home volume it runs by itself; this is for trying out an edit
// without resetting the environment. Unlike at boot, a failure does not
// stop the container.
func (a *app) runInit(ctx context.Context, env config.Environment, out, errOut io.Writer) (int, error) {
	eng, disconnect, err := a.dial(a.getenv)
	if err != nil {
		return 0, err
	}
	defer disconnect()
	dev, err := runningDev(ctx, eng, env)
	if err != nil {
		return 0, err
	}
	code, err := eng.Exec(ctx, dev.ID, engine.ExecSpec{Cmd: []string{guestBin, "init"}}, engine.Streams{Out: out, Err: errOut})
	if err != nil {
		return 0, err
	}
	if staleGuest(code) {
		return code, fmt.Errorf("the image of %q is older than this bh (bh-guest is missing or too old) — rebuild it with bh restart %s --rebuild", env.Name, env.Name)
	}
	return code, nil
}

func (a *app) initRun(ctx context.Context, args []string) error {
	ref, err := oneEnv("init", args)
	if err != nil {
		return err
	}
	_, env, err := a.localEnv("init", ref)
	if err != nil {
		return err
	}
	// bh-guest reports its own progress and "init.sh finished" on stderr.
	code, err := a.runInit(ctx, env, a.stdout, a.stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return exitCode(code)
	}
	return nil
}

// rootShellPath is the PATH of the root shell: the image's own system
// directories only. Anything the developer user can write to (the nix
// profile, the workspace) is kept out, or code inside the environment
// could place a program for root to run.
const rootShellPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// rootShellSpec is the administration console: a privileged root shell in
// dev, outside cap_drop ALL (a plain root could not chown, so apt would not
// work). Its home is /root, in the image, so it reads none of the
// developer's shell configuration.
func rootShellSpec(size func() (uint, uint)) engine.ExecSpec {
	return engine.ExecSpec{
		User: "root", Privileged: true, WorkingDir: "/root",
		Env: []string{"HOME=/root", "PATH=" + rootShellPath},
		Cmd: []string{"zsh"},
		TTY: true, Size: size,
	}
}

func (a *app) rootShell(ctx context.Context, args []string) error {
	ref, err := oneEnv("root-shell", args)
	if err != nil {
		return err
	}
	_, env, err := a.localEnv("root-shell", ref)
	if err != nil {
		return err
	}
	if !a.isTerminal() {
		return errors.New("root-shell needs a terminal")
	}
	eng, disconnect, err := a.dial(a.getenv)
	if err != nil {
		return err
	}
	defer disconnect()
	dev, err := runningDev(ctx, eng, env)
	if err != nil {
		return err
	}

	fmt.Fprintf(a.stderr, "root shell in %q — full privileges; changes outside the volumes are lost when the container is recreated\n", env.Name)
	restore, size, err := a.rawTerminal()
	if err != nil {
		return fmt.Errorf("preparing the terminal: %w", err)
	}
	code, err := eng.Exec(ctx, dev.ID, rootShellSpec(size), engine.Streams{In: a.stdin, Out: a.stdout, Err: a.stderr})
	restore()
	if err != nil {
		return err
	}
	if code != 0 {
		return exitCode(code)
	}
	return nil
}

func (a *app) gpu(ctx context.Context, args []string) error {
	if len(args) != 1 || args[0] != "list" {
		return usagef("usage: bh gpu list")
	}
	eng, disconnect, err := a.dial(a.getenv)
	if err != nil {
		return err
	}
	defer disconnect()
	devices, known, err := eng.CDIDevices(ctx)
	if err != nil {
		return err
	}
	if !known {
		return errors.New("this Docker engine does not report CDI devices (it takes Engine 28.2 or later with CDI enabled)")
	}
	if len(devices) == 0 {
		fmt.Fprintln(a.stdout, "No CDI devices found (generate a CDI spec with your GPU vendor's toolkit)")
		return nil
	}
	for _, device := range devices {
		fmt.Fprintln(a.stdout, device)
	}
	return nil
}
