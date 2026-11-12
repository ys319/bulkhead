package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/ops"
	"github.com/ys319/bulkhead/internal/sshconfig"
	"github.com/ys319/bulkhead/internal/sshkey"
)

// runTool runs a program of the host and returns what it printed. The
// error says what went wrong in the program's own words.
func runTool(name string, args []string) (string, error) {
	var out, errOut bytes.Buffer
	command := exec.Command(name, args...)
	command.Stdout, command.Stderr = &out, &errOut
	if err := command.Run(); err != nil {
		if detail := strings.TrimSpace(errOut.String()); detail != "" {
			return out.String(), fmt.Errorf("%s failed:\n%s", name, detail)
		}
		return out.String(), fmt.Errorf("%s: %w", name, err)
	}
	return out.String(), nil
}

// stdinIsTerminal: somebody is typing, as opposed to a pipe or a file.
func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// load reads the configuration directory.
func (a *app) load() (config.Store, *config.Config, error) {
	store, err := a.store()
	if err != nil {
		return store, nil, err
	}
	cfg, err := store.Load()
	return store, cfg, err
}

// sshConfigPath is the user's ssh client configuration. BH_SSH_CONFIG names
// another file, for trying bh out without touching the real one.
func (a *app) sshConfigPath() (string, error) {
	if explicit := a.getenv("BH_SSH_CONFIG"); explicit != "" {
		return explicit, nil
	}
	return sshconfig.DefaultPath(a.goos, a.getenv)
}

// syncSSH rewrites the Host entries from what is on disk now. The commands
// that change the set of environments, a name or a port call it last: by
// then their own work is done, so a failure here is a warning.
func (a *app) syncSSH(store config.Store) {
	if err := a.writeSSH(store); err != nil {
		fmt.Fprintf(a.stderr, "bh: warning: the ssh configuration was not updated: %v\n", err)
	}
}

func (a *app) writeSSH(store config.Store) error {
	path, err := a.sshConfigPath()
	if err != nil {
		return err
	}
	cfg, err := store.Load()
	if err != nil {
		return err
	}
	return sshconfig.Sync(path, store.MachineKey(), sshHosts(cfg))
}

// sshHosts are the environments as the ssh client reaches them.
func sshHosts(cfg *config.Config) []sshconfig.Host {
	hosts := make([]sshconfig.Host, 0, len(cfg.Envs))
	for _, env := range cfg.Envs {
		host := "127.0.0.1"
		if !env.Local() {
			host = env.Host
		}
		hosts = append(hosts, sshconfig.Host{Name: env.Name, HostName: host, Port: env.SSHPort})
	}
	return hosts
}

// forgetHostKey drops the environment's entry from known_hosts. Only
// destroying an environment calls for it: that is when its ssh host key
// goes, and the next environment on the same port would look like an
// attack to the ssh client. Nothing to forget is not a problem.
func (a *app) forgetHostKey(port int) {
	path, err := a.sshConfigPath()
	if err != nil {
		return
	}
	knownHosts := sshconfig.KnownHosts(path)
	if _, err := os.Stat(knownHosts); err != nil {
		return
	}
	// The file is named: ssh-keygen's own default comes from the password
	// database, not from HOME.
	_, _ = a.tool("ssh-keygen", []string{"-R", fmt.Sprintf("[127.0.0.1]:%d", port), "-f", knownHosts})
}

// inspect asks the engine about a local environment's containers.
func (a *app) inspect(ctx context.Context, env config.Environment) (ops.Status, error) {
	eng, disconnect, err := a.dial(a.getenv)
	if err != nil {
		return ops.Status{}, err
	}
	defer disconnect()
	return (ops.Runner{Engine: eng}).Inspect(ctx, env.ID)
}

// active: one of the containers runs. A half-running environment counts,
// so that it can still be stopped.
func active(status ops.Status) bool {
	return (status.Holder != nil && status.Holder.Running()) || (status.Dev != nil && status.Dev.Running())
}

// requireRunning says what is wrong instead of ssh's "Connection refused".
func (a *app) requireRunning(ctx context.Context, env config.Environment) error {
	if !env.Local() {
		return nil // its state cannot be seen from this machine's Docker
	}
	status, err := a.inspect(ctx, env)
	if err != nil {
		return err
	}
	if status.Dev == nil || !status.Dev.Running() {
		return fmt.Errorf("%q is not running (start it with bh up %s)", env.Name, env.Name)
	}
	return nil
}

var errAborted = errors.New("not confirmed — nothing was changed")

// ensureMachineKey returns this machine's public key, making the pair the
// first time. It is never replaced.
func ensureMachineKey(store config.Store) (string, error) {
	if err := os.MkdirAll(store.Dir, 0o755); err != nil {
		return "", err
	}
	publicKey, err := sshkey.EnsureMachineKey(store.MachineKey(), machineKeyComment)
	if err != nil {
		return "", fmt.Errorf("preparing the machine's ssh key: %w", err)
	}
	return publicKey, nil
}

// newHTTPClient is how bh reaches the services tokens come from. It goes
// through the proxy the environment variables name (HTTPS_PROXY, NO_PROXY),
// like every other tool on the machine, and gives up instead of hanging.
func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}
}

// openInBrowser hands an address to the desktop's default browser and does
// not wait for it. Where there is no browser (a machine reached over ssh)
// this fails; the callers have printed the address by then.
func openInBrowser(address string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", address)
	case "windows":
		// Not cmd's start: the & in an address would end the command there.
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", address)
	default:
		command = exec.Command("xdg-open", address)
	}
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
}
