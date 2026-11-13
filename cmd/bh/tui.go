package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/engine"
	"github.com/ys319/bulkhead/internal/ops"
	"github.com/ys319/bulkhead/internal/plan"
	"github.com/ys319/bulkhead/internal/snapshot"
	"github.com/ys319/bulkhead/internal/tui"
)

// interactive is bh without arguments: the full-screen interface.
func (a *app) interactive(ctx context.Context) error {
	if !a.isTerminal() {
		return usagef("bh without arguments opens the full-screen interface, which needs a terminal\n\n%s", usageText)
	}
	// ctx ends on Ctrl-C, which is right for a command and wrong here: the
	// interrupt is meant for the program that has the terminal at that
	// moment (bh logs, started from the menu), and the interface must still
	// be there when that program is gone. At the keyboard of the interface
	// itself Ctrl-C is a key.
	return a.fullScreen(context.WithoutCancel(ctx), backend{a})
}

// backend is what the full-screen interface acts through: the same
// functions as the commands, with their output caught instead of printed.
type backend struct{ a *app }

// quiet is the app with its output going to w.
func (b backend) quiet(w io.Writer) *app {
	silenced := *b.a
	silenced.stdout, silenced.stderr = w, w
	return &silenced
}

func (b backend) Load(ctx context.Context) (tui.Snapshot, error) {
	store, cfg, err := b.a.load()
	if err != nil {
		return tui.Snapshot{}, err
	}
	keys, keysErr := store.LoadKeys()
	if keysErr != nil {
		return tui.Snapshot{}, keysErr
	}
	snap := tui.Snapshot{Broken: cfg.Broken, Keys: keys}

	// One connection for all environments, and none when there is no
	// local one. When the engine cannot be asked the list still shows what
	// is configured.
	var eng dockerEngine
	for _, env := range cfg.Envs {
		state := tui.EnvState{Env: env}
		if env.Local() {
			if eng == nil && snap.EngineProblem == "" {
				dialed, disconnect, err := b.a.dial(b.a.getenv)
				if err != nil {
					snap.EngineProblem = err.Error()
				} else {
					defer disconnect()
					eng = dialed
				}
			}
			if eng != nil {
				status, err := (ops.Runner{Engine: eng}).Inspect(ctx, env.ID)
				if err != nil {
					snap.EngineProblem, eng = err.Error(), nil
				} else {
					state.Status, state.Known = status, true
					// The boundary as it is on the containers now: the same
					// findings bh doctor prints for this environment.
					state.Findings = checkEnvironment(ctx, eng, store, env, keys, nil)
				}
			}
			// Records only: the values stay where they are kept.
			if creds, err := b.a.credentialsOf(store, env, false); err != nil {
				state.TokenProblem = err.Error()
			} else {
				for _, token := range creds.Tokens {
					state.Tokens = append(state.Tokens, tui.Token{
						Service: token.Service, Summary: tokenSummary(token, b.a.now()), Urgency: token.Urgency(b.a.now()),
					})
				}
				for _, secret := range creds.Secrets {
					state.Secrets = append(state.Secrets, secret.Name)
				}
			}
		}
		snap.Envs = append(snap.Envs, state)
	}
	return snap, nil
}

func (b backend) HasVSCode() bool { return b.a.hasVSCode() }

func (b backend) Create(name, from string) error {
	_, _, err := b.quiet(io.Discard).newLocal(name, from, nil)
	return err
}

func (b backend) Rename(name, newName string) error {
	_, _, err := b.quiet(io.Discard).renameEnv(name, newName)
	return err
}

func (b backend) Up(ctx context.Context, name string, progress io.Writer) error {
	return b.quiet(progress).start(ctx, "up", name, nil, ops.Runner.Up)
}

func (b backend) Restart(ctx context.Context, name string, progress io.Writer) error {
	return b.quiet(progress).start(ctx, "restart", name, nil, ops.Runner.Restart)
}

func (b backend) Down(ctx context.Context, name string) error {
	return b.quiet(io.Discard).down(ctx, []string{name})
}

func (b backend) Reset(ctx context.Context, name string, home bool, progress io.Writer) (string, error) {
	done, err := b.quiet(progress).resetEnv(ctx, name, home, true)
	if err != nil {
		return "", err
	}
	return done.report(name) + " — Start runs init.sh again", nil
}

func (b backend) Destroy(ctx context.Context, name string) error {
	return b.quiet(io.Discard).destroy(ctx, []string{name, "--yes"})
}

func (b backend) Configure(ctx context.Context, name string, changes config.Changes) error {
	_, err := b.quiet(io.Discard).applyChanges(ctx, name, changes)
	return err
}

func (b backend) RunInit(ctx context.Context, name string, progress io.Writer) error {
	_, env, err := b.a.localEnv("init", name)
	if err != nil {
		return err
	}
	code, err := b.a.runInit(ctx, env, progress, progress)
	if err == nil && code != 0 {
		err = fmt.Errorf("init.sh failed (exit status %d)", code)
	}
	return err
}

func (b backend) Open(ctx context.Context, name string) error {
	store, cfg, err := b.a.load()
	if err != nil {
		return err
	}
	env, err := cfg.Find(name)
	if err != nil {
		return err
	}
	return b.a.openInEditor(ctx, store, env)
}

func (b backend) AddRemote(name, host string, port int) error {
	_, err := b.quiet(io.Discard).addRemote(name, host, port)
	return err
}

func (b backend) ConfigureRemote(name string, host *string, port *int) error {
	_, err := b.quiet(io.Discard).configureRemote(name, host, port)
	return err
}

func (b backend) RemoveRemote(name string) error {
	_, err := b.quiet(io.Discard).removeRemote(name)
	return err
}

// Reach logs in and runs `true`. BatchMode makes everything that would ask
// (a passphrase, a password) fail instead of waiting for input behind the
// full screen.
func (b backend) Reach(_ context.Context, name string) error {
	store, cfg, err := b.a.load()
	if err != nil {
		return err
	}
	env, err := cfg.Find(name)
	if err != nil {
		return err
	}
	args := append(sshArgs(store, env, "BatchMode=yes", "ConnectTimeout=5", "ForwardAgent=no"), "true")
	_, err = b.a.tool("ssh", args)
	return err
}

func (b backend) RegisterKey(input, label string) (config.KeyEntry, error) {
	store, err := b.a.store()
	if err != nil {
		return config.KeyEntry{}, err
	}
	if _, err := store.Init(); err != nil {
		return config.KeyEntry{}, err
	}
	return registerKey(store, input, label, true)
}

func (b backend) RenameKey(fingerprint, label string) error {
	store, err := b.a.store()
	if err != nil {
		return err
	}
	_, err = renameKey(store, fingerprint, label)
	return err
}

func (b backend) RemoveKey(fingerprint string) ([]string, error) {
	store, err := b.a.store()
	if err != nil {
		return nil, err
	}
	return removeKey(store, fingerprint)
}

func (b backend) AttachKey(name, fingerprint string) error {
	store, err := b.a.store()
	if err != nil {
		return err
	}
	_, _, err = attachKey(store, name, fingerprint)
	return err
}

func (b backend) DetachKey(name, fingerprint string) error {
	store, err := b.a.store()
	if err != nil {
		return err
	}
	_, _, err = detachKey(store, name, fingerprint)
	return err
}

func (b backend) MachineKey() (string, error) {
	store, err := b.a.store()
	if err != nil {
		return "", err
	}
	if _, err := store.Init(); err != nil {
		return "", err
	}
	return ensureMachineKey(store)
}

func (b backend) CDIDevices(ctx context.Context) ([]string, bool, error) {
	eng, disconnect, err := b.a.dial(b.a.getenv)
	if err != nil {
		return nil, false, err
	}
	defer disconnect()
	return eng.CDIDevices(ctx)
}

// Addresses are the names other machines may know this one by: its
// hostname, and the IPv4 address of every interface that is up (the holder
// turns IPv6 off, so only IPv4 leads into an environment).
func (b backend) Addresses() []tui.Address {
	var addresses []tui.Address
	if name, err := os.Hostname(); err == nil && name != "" {
		addresses = append(addresses, tui.Address{Host: name, Label: "hostname"})
	}
	interfaces, _ := net.Interfaces()
	for _, each := range interfaces {
		if each.Flags&net.FlagUp == 0 || each.Flags&net.FlagLoopback != 0 {
			continue
		}
		found, _ := each.Addrs()
		for _, address := range found {
			if network, ok := address.(*net.IPNet); ok && network.IP.To4() != nil {
				addresses = append(addresses, tui.Address{Host: network.IP.String(), Label: each.Name})
			}
		}
	}
	return addresses
}

func (b backend) Shell(name string) (*exec.Cmd, error) {
	store, cfg, err := b.a.load()
	if err != nil {
		return nil, err
	}
	env, err := cfg.Find(name)
	if err != nil {
		return nil, err
	}
	return exec.Command("ssh", sshArgs(store, env)...), nil
}

// Self is this program again, for the commands that take the terminal.
func (b backend) Self(args ...string) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("cannot find bh's own program file: %w", err)
	}
	return exec.Command(self, args...), nil
}

// Editor is the user's editor: VISUAL, then EDITOR (which may carry
// arguments, as in "code --wait"), then what every system has.
func (b backend) Editor(path string) (*exec.Cmd, error) {
	for _, variable := range []string{"VISUAL", "EDITOR"} {
		if words := strings.Fields(b.a.getenv(variable)); len(words) > 0 {
			return exec.Command(words[0], append(words[1:], path)...), nil
		}
	}
	if b.a.goos == "windows" {
		return exec.Command("notepad", path), nil
	}
	return exec.Command("vi", path), nil
}

func (b backend) InitScript() (string, error) {
	store, err := b.a.store()
	if err != nil {
		return "", err
	}
	return ensureInitScript(store)
}

// devDockerfileTemplate is what a new dev.Dockerfile starts as: the frame
// stays, the user adds steps between the markers. Left as it is, it builds
// the base image again under another name.
const devDockerfileTemplate = `# bulkhead dev image extension — edit between the markers.
# Applied on the next bh up / restart. Delete this file to revert.

FROM ` + devBaseRepo + `

USER root

### >>> your build steps >>>

# Runs as root. The build context is this directory (COPY reads files here).

# RUN apt-get update \
#   && apt-get install -y --no-install-recommends ffmpeg \
#   && rm -rf /var/lib/apt/lists/*

### <<< your build steps <<<

USER developer
`

func (b backend) DevDockerfile(name string) (string, error) {
	store, env, err := b.a.localEnv("dev.Dockerfile", name)
	if err != nil {
		return "", err
	}
	path := store.DevDockerfile(env.ID)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		err = os.WriteFile(path, []byte(devDockerfileTemplate), 0o644)
		return path, err
	} else if err != nil {
		return "", err
	}
	return path, nil
}

// TidyDevDockerfile removes a dev.Dockerfile that is still the template,
// byte for byte: it changes nothing about the image, but its presence alone
// would have every start build an image for this environment.
func (b backend) TidyDevDockerfile(name string) (bool, error) {
	store, env, err := b.a.localEnv("dev.Dockerfile", name)
	if err != nil {
		return false, err
	}
	path := store.DevDockerfile(env.ID)
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, []byte(devDockerfileTemplate)) {
		return false, nil
	}
	return true, os.Remove(path)
}

// Watch follows the engine's container events. When the connection breaks
// it says so, tries again with growing pauses, and reports a change once it
// is back — whatever happened meanwhile was missed.
func (b backend) Watch(ctx context.Context) <-chan bool {
	out := make(chan bool)
	send := func(ok bool) bool {
		select {
		case out <- ok:
			return true
		case <-ctx.Done():
			return false
		}
	}
	go func() {
		defer close(out)
		pause, lost := time.Second, false
		for ctx.Err() == nil {
			eng, disconnect, err := b.a.dial(b.a.getenv)
			if err == nil {
				changes, done := eng.Watch(ctx, plan.LabelEnv)
				if lost {
					lost = false
					if !send(true) {
						disconnect()
						return
					}
				}
				for range changes {
					pause = time.Second
					if !send(true) {
						disconnect()
						return
					}
				}
				err = <-done
				disconnect()
				if err == nil {
					return // the context ended
				}
			}
			if !lost {
				lost = true
				if !send(false) {
					return
				}
			}
			select {
			case <-time.After(pause):
			case <-ctx.Done():
				return
			}
			if pause *= 2; pause > 30*time.Second {
				pause = 30 * time.Second
			}
		}
	}()
	return out
}

func (b backend) FollowLogs(ctx context.Context, name string, tail int, out io.Writer) error {
	_, env, err := b.a.localEnv("logs", name)
	if err != nil {
		return err
	}
	eng, disconnect, err := b.a.dial(b.a.getenv)
	if err != nil {
		return err
	}
	defer disconnect()
	status, err := (ops.Runner{Engine: eng}).Inspect(ctx, env.ID)
	if err != nil {
		return err
	}
	return streamLogs(ctx, eng, status, engine.LogOptions{Follow: true, Tail: tail}, out)
}

// pickable are the commands that ask which environment when none is named
// (at a terminal), and whether they are for local environments only.
var pickable = map[string]bool{
	"up": true, "down": true, "restart": true, "logs": true,
	"status": false, "ssh": false, "open": false,
}

// askWhich lets the user choose the environment a command was not given.
func (a *app) askWhich(ctx context.Context, command string, localOnly bool) (string, error) {
	_, cfg, err := a.load()
	if err != nil {
		return "", err
	}
	var choices []tui.Choice
	for _, env := range cfg.Envs {
		switch {
		case env.Local():
			choices = append(choices, tui.Choice{Name: env.Name, Note: sshAddress(env)})
		case !localOnly:
			choices = append(choices, tui.Choice{Name: env.Name, Note: "remote, " + sshAddress(env)})
		}
	}
	if len(choices) == 0 {
		return "", usagef("usage: bh %s <env> (there is no environment to choose from yet)", command)
	}
	name, err := a.pick(ctx, "bh "+command+" — which environment?", choices)
	if errors.Is(err, tui.ErrNothingPicked) {
		return "", exitCode(1)
	}
	return name, err
}

func (b backend) DiskUsage(ctx context.Context) (map[string]int64, error) {
	eng, disconnect, err := b.a.dial(b.a.getenv)
	if err != nil {
		return nil, err
	}
	defer disconnect()
	return eng.VolumeSizes(ctx)
}

// said joins what an operation reports into the lines of one message.
func said(first string, notes, warnings []string) string {
	return strings.Join(append(append([]string{first}, notes...), warnings...), "\n")
}

func (b backend) SetVar(ctx context.Context, name, key, value string) (string, error) {
	result, err := b.quiet(io.Discard).changeVar(ctx, name, key, value, false)
	if err != nil {
		return "", err
	}
	return handedIn("set "+key, result.Env, result.SyncErr), nil
}

func (b backend) UnsetVar(ctx context.Context, name, key string) (string, error) {
	result, err := b.quiet(io.Discard).changeVar(ctx, name, key, "", true)
	if err != nil {
		return "", err
	}
	return handedIn("took "+key+" out", result.Env, result.SyncErr), nil
}

func (b backend) SetSecret(ctx context.Context, name, key, value string) (string, error) {
	result, err := b.quiet(io.Discard).setSecret(ctx, name, key, value)
	if err != nil {
		return "", err
	}
	return handedIn("set the secret "+key, result.Env, result.SyncErr), nil
}

func (b backend) RemoveSecret(ctx context.Context, name, key string) (string, error) {
	result, err := b.quiet(io.Discard).removeSecret(ctx, name, key)
	if err != nil {
		return "", err
	}
	return handedIn("took the secret "+key+" out", result.Env, result.SyncErr), nil
}

// handedIn words a change of what an environment is handed.
func handedIn(done string, env config.Environment, syncErr error) string {
	if syncErr != nil {
		return done + "\n" + notYet(env, syncErr)
	}
	return done + " — " + newShells
}

func (b backend) TokenServices() []tui.TokenService {
	services := make([]tui.TokenService, len(tokenServices))
	for i, service := range tokenServices {
		services[i] = tui.TokenService{Name: service.Name, Title: service.Title}
		for _, preset := range service.Presets {
			services[i].Presets = append(services[i].Presets, tui.TokenPreset{Name: preset.Name, About: preset.About})
		}
	}
	return services
}

func (b backend) TokenIssue(name, serviceName, preset string, refresh bool) (tui.TokenIssue, error) {
	service, known := serviceNamed(serviceName)
	if !known {
		return tui.TokenIssue{}, fmt.Errorf("%q is not a service bh gets tokens from", serviceName)
	}
	var issue tokenIssue
	if refresh {
		store, err := b.a.store()
		if err != nil {
			return tui.TokenIssue{}, err
		}
		env, err := handedTo(store, name)
		if err != nil {
			return tui.TokenIssue{}, err
		}
		creds, err := b.a.credentialsOf(store, env, false)
		if err != nil {
			return tui.TokenIssue{}, err
		}
		token := creds.token(service.Name)
		if token == nil {
			return tui.TokenIssue{}, fmt.Errorf("%q has no %s token to refresh", name, service.Title)
		}
		issue = reissueFor(service, *token)
	} else {
		var err error
		if issue, err = b.a.issueFor(service, name, githubRequest{Preset: preset}); err != nil {
			return tui.TokenIssue{}, err
		}
	}
	return tui.TokenIssue{Address: issue.URL, Steps: issue.Steps, TokenName: issue.TokenName}, nil
}

func (b backend) SetToken(ctx context.Context, name, serviceName, preset, value, tokenName string, refresh bool) (string, error) {
	service, known := serviceNamed(serviceName)
	if !known {
		return "", fmt.Errorf("%q is not a service bh gets tokens from", serviceName)
	}
	a := b.quiet(io.Discard)
	request := githubRequest{Preset: preset}
	if refresh {
		// What bh asked for stays as recorded.
		store, err := a.store()
		if err != nil {
			return "", err
		}
		env, err := handedTo(store, name)
		if err != nil {
			return "", err
		}
		creds, err := a.credentialsOf(store, env, false)
		if err != nil {
			return "", err
		}
		if token := creds.token(service.Name); token != nil {
			request.Preset, request.Owner, tokenName = token.Preset, token.Owner, token.TokenName
		}
	}
	result, err := a.setToken(ctx, name, service, value, request, tokenName, refresh)
	if err != nil {
		return "", err
	}
	notes, warnings := result.report()
	return said(service.Title+" token: "+tokenSummary(result.Token, b.a.now()), notes, warnings), nil
}

func (b backend) RemoveToken(ctx context.Context, name, serviceName string) (string, error) {
	service, known := serviceNamed(serviceName)
	if !known {
		return "", fmt.Errorf("%q is not a service bh gets tokens from", serviceName)
	}
	result, err := b.quiet(io.Discard).removeToken(ctx, name, service)
	if err != nil {
		return "", err
	}
	text := "revoked the " + service.Title + " token and took it out"
	if result.SyncErr != nil {
		text += fmt.Sprintf("\nit is still in the environment (%s) — bh up %s takes it out; it no longer works", oneLine(result.SyncErr), result.Env.Name)
	}
	return text, nil
}

func (b backend) OpenURL(address string) error { return b.a.openURL(address) }

func (b backend) Snapshot(ctx context.Context, name string, progress io.Writer) (string, error) {
	taken, _, err := b.quiet(progress).createSnapshot(ctx, name, snapshot.KindManual, "", "")
	return strings.TrimSuffix(filepath.Base(taken.Path), snapshot.Extension), err
}

// Snapshots lists the environment's snapshots, newest first.
func (b backend) Snapshots(name string) ([]tui.SnapshotEntry, error) {
	store, cfg, err := b.a.load()
	if err != nil {
		return nil, err
	}
	env, err := cfg.Find(name)
	if err != nil {
		return nil, err
	}
	found, err := listSnapshots(store, env.ID)
	if err != nil {
		return nil, err
	}
	var entries []tui.SnapshotEntry
	for _, s := range slices.Backward(found) {
		if s.Err != nil {
			continue
		}
		about := fmt.Sprintf("%s  %s  %s", s.Manifest.Created.Local().Format("2006-01-02 15:04"), tui.HumanBytes(s.Size), s.Manifest.Kind)
		if s.Manifest.Note != "" {
			about += "  " + s.Manifest.Note
		}
		entries = append(entries, tui.SnapshotEntry{Name: s.Name, About: about})
	}
	return entries, nil
}

func (b backend) Rollback(ctx context.Context, name, snapshotName string, progress io.Writer) (string, error) {
	a := b.quiet(progress)
	store, planned, err := a.planRollback(name, snapshotName, "")
	if err != nil {
		return "", err
	}
	kept, err := a.rollBack(ctx, store, planned, true)
	return strings.TrimSuffix(kept, snapshot.Extension), err
}

func (b backend) Archive(ctx context.Context, name string, progress io.Writer) (string, error) {
	result, err := b.quiet(progress).archiveEnv(ctx, name)
	if err != nil {
		return "", err
	}
	text := fmt.Sprintf("%q is archived", name)
	if len(result.Revoked) > 0 {
		text += " and its tokens revoked (" + strings.Join(result.Revoked, ", ") + ")"
	}
	if len(result.Warnings) > 0 {
		text += " — but: " + strings.Join(result.Warnings, "; ")
	}
	return text, nil
}

func (b backend) Unarchive(ctx context.Context, name string, progress io.Writer) error {
	_, err := b.quiet(progress).unarchiveEnv(ctx, name)
	return err
}
