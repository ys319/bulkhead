package tui

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/ys319/bulkhead/internal/config"
)

// The menus. Names follow one rule: what opens another screen is a noun,
// what acts at once is a verb. What cannot be done in the environment's
// current state is not listed — except settings that are locked while it
// runs, which are shown in gray so that their values can still be read.

var separator = item{separator: true}

// choose opens what Enter means for a row of the list.
func (m *Model) choose(r row) tea.Cmd {
	switch r.kind {
	case rowEnv:
		if r.env.Env.Local() {
			m.push(m.envMenu(r.env.Env.ID, r.env.Env.Name))
		} else {
			m.push(m.remoteMenu(r.env.Env.ID, r.env.Env.Name))
		}
	case rowBroken:
		id := r.broken.ID
		m.push(&menu{name: id, note: []string{"Its settings cannot be read. Fix the file and press r, or remove the environment."},
			build: func(*Model) []item {
				return []item{{label: "Destroy", color: red, run: func(m *Model) tea.Cmd { return m.askDestroy(id, true) }}}
			}})
	case rowKey:
		m.push(m.keyMenu(r.key.Fingerprint, sanitize(r.key.DisplayName())))
	case rowCreate:
		m.push(m.askName("Create environment", "Name of the new environment:", "", func(m *Model, name string) tea.Cmd {
			return m.create(name, "")
		}))
	case rowAddRemote:
		m.push(m.askRemote())
	case rowRegisterKey:
		m.push(m.askKey())
	case rowSettings:
		m.push(&menu{name: "Settings", build: func(*Model) []item {
			return []item{{label: "Edit init.sh", run: func(m *Model) tea.Cmd {
				path, err := m.backend.InitScript()
				if err != nil {
					return m.fail(err)
				}
				return m.edit("the editor", path, nil)
			}}}
		}})
	case rowMachineKey:
		return m.showMachineKey()
	}
	return nil
}

// fail closes the dialogs and shows the error.
func (m *Model) fail(err error) tea.Cmd {
	m.closeAll()
	m.say(red, "%s", err.Error())
	return nil
}

func (m *Model) envMenu(id, name string) *menu {
	return &menu{name: name, build: func(m *Model) []item {
		state := m.env(id)
		if state == nil {
			return nil
		}
		name := state.Env.Name
		if state.Env.Archived != "" {
			return []item{
				{label: "Unarchive", run: func(m *Model) tea.Cmd {
					return m.do("Bringing "+name+" back", fmt.Sprintf("%q is back — Start runs init.sh and installs the tools", name), false, func(progress io.Writer) error {
						return m.backend.Unarchive(m.ctx, name, progress)
					})
				}},
				separator,
				{label: "Rename", run: func(m *Model) tea.Cmd { m.push(m.askRename(name)); return nil }},
				{label: "Destroy", color: red, run: func(m *Model) tea.Cmd { return m.askDestroy(name, true) }},
			}
		}
		var items []item
		if state.Known && active(*state) {
			items = append(items,
				item{label: "Stop", run: func(m *Model) tea.Cmd {
					return m.do("Stopping "+name, fmt.Sprintf("%q is stopped", name), false, func(io.Writer) error {
						return m.backend.Down(m.ctx, name)
					})
				}},
				// Yellow: recreating drops what lives in the containers
				// themselves. It asks first.
				item{label: "Reboot", color: yellow, run: func(m *Model) tea.Cmd {
					m.push(&confirm{name: "Reboot " + name, color: yellow,
						text: []string{"Recreate the containers and start again?", "Everything outside the volumes (home, workspace, /nix) is lost — for example packages installed from the root shell. Open sessions end."},
						onYes: func(m *Model) tea.Cmd {
							return m.do("Rebooting "+name, fmt.Sprintf("%q is up again", name), false, func(progress io.Writer) error {
								return m.backend.Restart(m.ctx, name, progress)
							})
						}})
					return nil
				}})
		} else {
			items = append(items, item{label: "Start", run: func(m *Model) tea.Cmd {
				return m.do("Starting "+name, fmt.Sprintf("%q is up", name), false, func(progress io.Writer) error {
					return m.backend.Up(m.ctx, name, progress)
				})
			}})
		}
		items = append(items, separator)
		if state.Known && devRunning(*state) {
			items = append(items, m.connectItems(name)...)
			items = append(items, separator)
		}
		items = append(items,
			item{label: "Settings", run: func(m *Model) tea.Cmd { m.push(m.settingsMenu(id, name)); return nil }},
			item{label: "Maintenance", run: func(m *Model) tea.Cmd { m.push(m.maintenanceMenu(id, name)); return nil }},
			separator,
			item{label: "Replicate", run: func(m *Model) tea.Cmd {
				m.push(m.askName("Replicate "+name, "Name of the copy (its settings and dev.Dockerfile are copied, its data is not):", "", func(m *Model, copyName string) tea.Cmd {
					return m.create(copyName, name)
				}))
				return nil
			}},
			item{label: "Rename", run: func(m *Model) tea.Cmd { m.push(m.askRename(name)); return nil }})
		// Reset removes volumes; without containers there is nothing to reset.
		if state.Known && created(*state) {
			items = append(items, item{label: "Reset", color: yellow, run: func(m *Model) tea.Cmd {
				m.push(m.resetMenu(name))
				return nil
			}})
		}
		return append(items, item{label: "Destroy", color: red, run: func(m *Model) tea.Cmd { return m.askDestroy(name, true) }})
	}}
}

// connectItems are the ways into a running (or remote) environment.
func (m *Model) connectItems(name string) []item {
	items := []item{{label: "Open shell", run: func(m *Model) tea.Cmd {
		command, err := m.backend.Shell(name)
		// ssh reserves 255 for its own failures; any other status is the
		// remote shell's and none of our business.
		return m.shellOut("ssh to "+name, command, err, func(err error) bool {
			var exit *exec.ExitError
			return errors.As(err, &exit) && exit.ExitCode() == 255
		}, nil)
	}}}
	if m.backend.HasVSCode() {
		items = append(items, item{label: "Open in VS Code", run: func(m *Model) tea.Cmd {
			return m.do("Opening "+name, fmt.Sprintf("opened %q in VS Code", name), false, func(io.Writer) error {
				return m.backend.Open(m.ctx, name)
			})
		}})
	}
	if m.backend.HasClaude() {
		items = append(items, item{label: "Open in Claude", run: func(m *Model) tea.Cmd {
			// The desktop asks before it connects: nothing is open yet.
			return m.do("Opening "+name, fmt.Sprintf("handed %q to Claude Desktop — confirm the connection there", name), false, func(io.Writer) error {
				return m.backend.OpenInClaude(m.ctx, name)
			})
		}})
	}
	return items
}

// settingsMenu: in the order of "worst to change while it runs".
func (m *Model) settingsMenu(id, name string) *menu {
	return &menu{name: name + " · Settings", build: func(m *Model) []item {
		return []item{
			{label: "Runtime", run: func(m *Model) tea.Cmd { m.push(m.runtimeForm(id)); return nil }},
			{label: "SSH", run: func(m *Model) tea.Cmd { m.push(m.sshMenu(id)); return nil }},
			// What is handed in needs no restart: new shells see it at once.
			{label: "Variables", run: func(m *Model) tea.Cmd { m.push(m.variablesMenu(id)); return nil }},
			{label: "Secrets", run: func(m *Model) tea.Cmd { m.push(m.secretsMenu(id)); return nil }},
			{label: "Tokens", run: func(m *Model) tea.Cmd { m.push(m.tokensMenu(id)); return nil }},
		}
	}}
}

func (m *Model) maintenanceMenu(id, name string) *menu {
	return &menu{name: name + " · Maintenance", build: func(m *Model) []item {
		state := m.env(id)
		if state == nil {
			return nil
		}
		name := state.Env.Name
		running := state.Known && devRunning(*state)
		exists := state.Known && created(*state)
		var items []item
		if running {
			items = append(items, item{label: "Root Shell", run: func(m *Model) tea.Cmd {
				command, err := m.backend.Self("root-shell", name)
				// Its status is the inner shell's own.
				return m.shellOut("the root shell", command, err, func(error) bool { return false }, nil)
			}})
		}
		if exists {
			items = append(items, item{label: "Logs", run: func(m *Model) tea.Cmd {
				command, err := m.backend.Self("logs", name)
				// Following has no end of its own, and nothing on the plain
				// terminal says how to leave it.
				return m.handOver(&handover{what: "logs", command: command,
					intro: fmt.Sprintf("Following the logs of %q — Ctrl-C returns to bh.", name)}, err, nil)
			}})
		}
		items = append(items, item{label: "Edit Dockerfile", run: func(m *Model) tea.Cmd {
			path, err := m.backend.DevDockerfile(name)
			if err != nil {
				return m.fail(err)
			}
			return m.edit("the editor", path, func(m *Model) {
				// A template left as it was would only cost a build.
				if removed, err := m.backend.TidyDevDockerfile(name); err == nil && removed {
					m.say(green, "dev.Dockerfile was left unchanged and removed again")
				} else if err == nil {
					m.say(green, "dev.Dockerfile applies at the next Start or Reboot of %q", name)
				}
			})
		}})
		if exists {
			items = append(items, item{label: "Rebuild", run: func(m *Model) tea.Cmd {
				m.push(&confirm{name: "Rebuild " + name, color: yellow,
					text: []string{"Build the image again from scratch (newer base image and packages), then reboot?", "This takes minutes. Other environments pick the new image up at their next Reboot."},
					onYes: func(m *Model) tea.Cmd {
						command, err := m.backend.Self("restart", name, "--rebuild", "--pull", "--no-cache")
						return m.shellOut("the rebuild", command, err, nil, nil)
					}})
				return nil
			}})
		}
		if running {
			items = append(items, item{label: "Rerun init.sh", run: func(m *Model) tea.Cmd {
				return m.do("Running init.sh in "+name, "init.sh finished", false, func(progress io.Writer) error {
					return m.backend.RunInit(m.ctx, name, progress)
				})
			}})
		}
		if exists {
			// The data can be read and replaced only while nothing uses it.
			busy := ""
			if state.Known && active(*state) {
				busy = fmt.Sprintf("stop %q first", name)
			}
			items = append(items, separator,
				item{label: "Snapshot", disabled: busy, run: func(m *Model) tea.Cmd {
					return m.doThen("Taking a snapshot of "+name, false, func(progress io.Writer) (string, string, error) {
						taken, err := m.backend.Snapshot(m.ctx, name, progress)
						return fmt.Sprintf("snapshot %s of %q taken", taken, name), "", err
					})
				}},
				item{label: "Roll back", color: yellow, disabled: busy, run: func(m *Model) tea.Cmd {
					m.push(m.rollbackMenu(name))
					return nil
				}},
				item{label: "Archive", color: yellow, disabled: busy, run: func(m *Model) tea.Cmd {
					m.push(&confirm{name: "Archive " + name, color: yellow,
						text: []string{"Take a snapshot, then remove the containers and volumes?", "The environment stays in the list. Unarchive brings the workspace and the home directory back; the nix store and caches do not come back — init.sh installs the tools again. A GitHub token of the environment is revoked."},
						onYes: func(m *Model) tea.Cmd {
							return m.doThen("Archiving "+name, false, func(progress io.Writer) (string, string, error) {
								text, err := m.backend.Archive(m.ctx, name, progress)
								return text, "", err
							})
						}})
					return nil
				}})
		}
		return items
	}}
}

// locked is why a setting cannot change now: the containers were created
// from it, and until they are recreated the settings would say one thing
// and Docker another.
func locked(state EnvState, what string) string {
	if state.Known && active(state) {
		return fmt.Sprintf("stop %q to change %s", state.Env.Name, what)
	}
	return ""
}

func onOff(value bool) string {
	if value {
		return "on"
	}
	return "off"
}

// sshMenu: each item is saved when it is chosen, and the menu shows the new
// value.
func (m *Model) sshMenu(id string) *menu {
	return &menu{name: "SSH", build: func(m *Model) []item {
		state := m.env(id)
		if state == nil {
			return nil
		}
		env := state.Env
		items := []item{
			{label: "Open to LAN", value: onOff(env.Publish), disabled: locked(*state, "Open to LAN"), run: func(m *Model) tea.Cmd {
				publish := !env.Publish
				return m.do("Saving", "", true, func(io.Writer) error {
					return m.backend.Configure(m.ctx, env.Name, config.Changes{Publish: &publish})
				})
			}},
			{label: "Port", value: strconv.Itoa(env.SSHPort), disabled: locked(*state, "Port"), run: func(m *Model) tea.Cmd {
				m.push(&input{name: "Port", prompt: []string{"The ssh port of " + env.Name + " on this machine:"}, value: []rune(strconv.Itoa(env.SSHPort)),
					submit: func(m *Model, value string) (tea.Cmd, error) {
						port, err := strconv.Atoi(strings.TrimSpace(value))
						if err != nil || port < 1 || port > 65535 {
							return nil, errors.New("a port is a number between 1 and 65535")
						}
						m.pop()
						return m.do("Saving", "", true, func(io.Writer) error {
							return m.backend.Configure(m.ctx, env.Name, config.Changes{SSHPort: &port})
						}), nil
					}})
				return nil
			}},
			{label: "Authorized Keys", value: strconv.Itoa(len(env.AuthorizedKeys)), run: func(m *Model) tea.Cmd {
				m.push(m.authorizedKeysMenu(id))
				return nil
			}},
		}
		// A loopback address is of no use to another machine.
		if env.Publish {
			items = append(items, item{label: "Copy address", run: func(m *Model) tea.Cmd { m.push(m.addressMenu(env)); return nil }})
		}
		return items
	}}
}

// addressMenu offers the ways other machines reach this one; the chosen
// one is copied as host:port/name, which is what + Add remote environment
// takes on the other machine.
func (m *Model) addressMenu(env config.Environment) *menu {
	return &menu{name: "Copy address", note: []string{"How does the other machine reach this one?"}, build: func(m *Model) []item {
		var items []item
		for _, candidate := range m.backend.Addresses() {
			text := config.RemoteAddress{Host: candidate.Host, Port: env.SSHPort, Name: env.Name}.String()
			items = append(items, item{label: candidate.Host, value: candidate.Label, run: func(m *Model) tea.Cmd {
				m.pop()
				m.say(green, "sent %s to the clipboard", text)
				return tea.SetClipboard(text)
			}})
		}
		return items
	}}
}

// authorizedKeysMenu attaches and detaches registered keys. Registering one
// happens in the Keys section of the list.
func (m *Model) authorizedKeysMenu(id string) *menu {
	return &menu{name: "Authorized Keys", note: []string{"This machine's key always gets in. Others:"}, build: func(m *Model) []item {
		state := m.env(id)
		if state == nil {
			return nil
		}
		env := state.Env
		names := map[string]string{}
		for _, key := range m.snap.Keys {
			names[key.Fingerprint] = sanitize(key.DisplayName())
		}
		apply := ""
		if state.Known && active(*state) {
			apply = " — reboot to apply"
		}
		var items []item
		for i, fingerprint := range env.AuthorizedKeys {
			name, registered := names[fingerprint]
			if !registered {
				name = "(missing from the registry) " + fingerprint
			}
			items = append(items, item{label: fmt.Sprintf("Detach %d: %s", i+1, name), run: func(m *Model) tea.Cmd {
				m.push(&confirm{name: "Detach key", color: red,
					text: []string{fmt.Sprintf("Stop letting %s into %q?", name, env.Name)},
					onYes: func(m *Model) tea.Cmd {
						m.pop()
						return m.do("Saving", "detached "+name+apply, true, func(io.Writer) error {
							return m.backend.DetachKey(env.Name, fingerprint)
						})
					}})
				return nil
			}})
		}
		return append(items, item{label: "+ Attach key", run: func(m *Model) tea.Cmd {
			var free []config.KeyEntry
			for _, key := range m.snap.Keys {
				if !slices.Contains(env.AuthorizedKeys, key.Fingerprint) {
					free = append(free, key)
				}
			}
			if len(free) == 0 {
				m.closeAll()
				m.say(red, "no registered key is left to attach — register one under Keys → + Register key")
				return nil
			}
			m.push(&menu{name: "Attach key", build: func(*Model) []item {
				var choices []item
				for _, key := range free {
					choices = append(choices, item{label: sanitize(key.DisplayName()), run: func(m *Model) tea.Cmd {
						m.pop()
						return m.do("Saving", "attached "+sanitize(key.DisplayName())+apply, true, func(io.Writer) error {
							return m.backend.AttachKey(env.Name, key.Fingerprint)
						})
					}})
				}
				return choices
			}})
			return nil
		}})
	}}
}

// runtimeForm collects changes and saves them together. While the
// environment runs it can be opened and read, but nothing in it changes.
func (m *Model) runtimeForm(id string) *menu {
	state := m.env(id)
	if state == nil {
		return &menu{name: "Runtime", build: func(*Model) []item { return nil }}
	}
	env := state.Env
	restart, gpu, socket := env.Restart, env.GPU, env.DockerSocket
	devices := slices.Clone(env.CDIDevices)
	next := func(values []string, current string, direction int) string {
		index := slices.Index(values, current)
		return values[(index+direction+len(values))%len(values)]
	}
	return &menu{name: "Runtime", form: true, build: func(m *Model) []item {
		state := m.env(id)
		if state == nil {
			return nil
		}
		lock := func(what string) string { return locked(*state, what) }
		items := []item{
			{label: "Restart", value: restart, disabled: lock("Restart"), cycle: func(d int) {
				restart = next([]string{"unless-stopped", "no"}, restart, d)
			}},
			{label: "GPU", value: gpu, disabled: lock("GPU"), cycle: func(d int) {
				gpu = next([]string{"off", "legacy", "cdi"}, gpu, d)
			}},
		}
		if gpu == "cdi" {
			items = append(items, item{label: "Attach GPU", value: strconv.Itoa(len(devices)), disabled: lock("GPU"), run: func(m *Model) tea.Cmd {
				m.push(m.devicePicker(&devices))
				return nil
			}})
		}
		return append(items,
			item{label: "Docker socket", value: onOff(socket), disabled: lock("Docker socket"), cycle: func(int) { socket = !socket }},
			separator,
			item{label: "Save and close", color: green, disabled: lock("Runtime settings"), run: func(m *Model) tea.Cmd {
				if gpu == "cdi" && len(devices) == 0 {
					m.say(yellow, "choose at least one device under Attach GPU, or set GPU to something else")
					return nil
				}
				changes := config.Changes{Restart: &restart, GPU: &gpu, DockerSocket: &socket, CDIDevices: &devices}
				return m.do("Saving", fmt.Sprintf("saved — applies at the next Start of %q", env.Name), false, func(io.Writer) error {
					return m.backend.Configure(m.ctx, env.Name, changes)
				})
			}})
	}}
}

// devicePicker chooses GPU devices: those the engine offers, plus names
// typed in (for an engine that does not list them).
func (m *Model) devicePicker(chosen *[]string) *menu {
	offered, _, _ := m.backend.CDIDevices(m.ctx)
	return &menu{name: "Attach GPU", form: true, build: func(m *Model) []item {
		names := slices.Clone(offered)
		for _, device := range *chosen {
			if !slices.Contains(names, device) {
				names = append(names, device)
			}
		}
		var items []item
		for _, device := range names {
			mark := "[ ] "
			if slices.Contains(*chosen, device) {
				mark = "[x] "
			}
			items = append(items, item{label: mark + device, cycle: func(int) {
				if at := slices.Index(*chosen, device); at >= 0 {
					*chosen = slices.Delete(*chosen, at, at+1)
				} else {
					*chosen = append(*chosen, device)
				}
			}})
		}
		return append(items,
			item{label: "+ Enter name", run: func(m *Model) tea.Cmd {
				m.push(&input{name: "Device name", prompt: []string{"A CDI device name, for example nvidia.com/gpu=0:"},
					submit: func(m *Model, value string) (tea.Cmd, error) {
						value = strings.TrimSpace(value)
						if !strings.Contains(value, "/") || !strings.Contains(value, "=") {
							return nil, errors.New("a CDI device name looks like vendor/class=name")
						}
						if !slices.Contains(*chosen, value) {
							*chosen = append(*chosen, value)
						}
						m.pop()
						return nil, nil
					}})
				return nil
			}},
			separator,
			item{label: "Done", color: green, run: func(m *Model) tea.Cmd { m.pop(); return nil }})
	}}
}

func (m *Model) remoteMenu(id, name string) *menu {
	return &menu{name: name, build: func(m *Model) []item {
		state := m.env(id)
		if state == nil {
			return nil
		}
		env := state.Env
		items := append(m.connectItems(env.Name), separator,
			item{label: "Configure", run: func(m *Model) tea.Cmd {
				current := config.RemoteAddress{Host: env.Host, Port: env.SSHPort, Name: env.Name}
				m.push(&input{name: "Configure " + env.Name, prompt: []string{"Where it is reached, as host:port —"}, value: []rune(env.Host + ":" + strconv.Itoa(env.SSHPort)),
					submit: func(m *Model, value string) (tea.Cmd, error) {
						address, err := config.ParseRemoteAddress(strings.TrimSpace(value) + "/" + current.Name)
						if err != nil {
							return nil, errors.New("expected host:port, for example studio.test:22001")
						}
						// What was known about reaching the old address says nothing now.
						delete(m.reach, id)
						return m.do("Saving", fmt.Sprintf("%q is now reached at %s:%d", env.Name, address.Host, address.Port), false, func(io.Writer) error {
							return m.backend.ConfigureRemote(env.Name, &address.Host, &address.Port)
						}), nil
					}})
				return nil
			}},
			item{label: "Rename", run: func(m *Model) tea.Cmd { m.push(m.askRename(env.Name)); return nil }},
			separator,
			// Only the pointer goes, and adding it again brings it back:
			// Remove, not Destroy, and a plain yes/no.
			item{label: "Remove", color: red, run: func(m *Model) tea.Cmd {
				m.push(&confirm{name: "Remove " + env.Name, color: red,
					text: []string{fmt.Sprintf("Forget the remote environment %q? Nothing on %s is touched.", env.Name, env.Host)},
					onYes: func(m *Model) tea.Cmd {
						return m.do("Removing", fmt.Sprintf("removed %q", env.Name), false, func(io.Writer) error {
							return m.backend.RemoveRemote(env.Name)
						})
					}})
				return nil
			}})
		return items
	}}
}

func (m *Model) keyMenu(fingerprint, name string) *menu {
	return &menu{name: name, build: func(m *Model) []item {
		var entry *config.KeyEntry
		for i := range m.snap.Keys {
			if m.snap.Keys[i].Fingerprint == fingerprint {
				entry = &m.snap.Keys[i]
			}
		}
		if entry == nil {
			return nil
		}
		users := m.usersOf(fingerprint)
		return []item{
			{label: "Rename", run: func(m *Model) tea.Cmd {
				m.push(&input{name: "Rename key", prompt: []string{"The name shown for this key:"}, value: []rune(entry.Label),
					submit: func(m *Model, value string) (tea.Cmd, error) {
						if strings.TrimSpace(value) == "" {
							return nil, errors.New("give the key a name")
						}
						return m.do("Saving", "renamed", false, func(io.Writer) error {
							return m.backend.RenameKey(fingerprint, value)
						}), nil
					}})
				return nil
			}},
			{label: "Environments", value: strconv.Itoa(len(users)), run: func(m *Model) tea.Cmd {
				m.push(m.keyEnvironments(fingerprint))
				return nil
			}},
			separator,
			{label: "Remove", color: red, run: func(m *Model) tea.Cmd {
				text := []string{fmt.Sprintf("Remove %s from the registry?", sanitize(entry.DisplayName()))}
				if len(users) > 0 {
					text = append(text, fmt.Sprintf("It is detached from %s first; the machine holding the key can no longer log in there once they restart.", strings.Join(users, ", ")))
				}
				m.push(&confirm{name: "Remove key", color: red, text: text, onYes: func(m *Model) tea.Cmd {
					// What was confirmed is this list of environments. If
					// it changed meanwhile, ask again rather than detach
					// from one nobody agreed to.
					if !slices.Equal(m.usersOf(fingerprint), users) {
						return m.fail(errors.New("the environments this key is attached to changed — nothing was removed; open the menu again"))
					}
					return m.do("Removing", "removed the key", false, func(io.Writer) error {
						_, err := m.backend.RemoveKey(fingerprint)
						return err
					})
				}})
				return nil
			}},
		}
	}}
}

// keyEnvironments chooses the local environments a key gets into; only the
// differences are applied.
func (m *Model) keyEnvironments(fingerprint string) *menu {
	chosen := m.usersOf(fingerprint)
	return &menu{name: "Environments", form: true, build: func(m *Model) []item {
		var items []item
		for _, state := range m.snap.Envs {
			if !state.Env.Local() {
				continue
			}
			name := state.Env.Name
			mark := "[ ] "
			if slices.Contains(chosen, name) {
				mark = "[x] "
			}
			items = append(items, item{label: mark + name, cycle: func(int) {
				if at := slices.Index(chosen, name); at >= 0 {
					chosen = slices.Delete(chosen, at, at+1)
				} else {
					chosen = append(chosen, name)
				}
			}})
		}
		if len(items) == 0 {
			return []item{{label: "(no local environments)", disabled: "create an environment first"}}
		}
		return append(items, separator, item{label: "Save and close", color: green, run: func(m *Model) tea.Cmd {
			before := m.usersOf(fingerprint)
			running := false
			// The work runs beside the interface: it gets its own copy of the list.
			envs := slices.Clone(m.snap.Envs)
			return m.doThen("Saving", false, func(io.Writer) (string, string, error) {
				for _, state := range envs {
					name := state.Env.Name
					was, now := slices.Contains(before, name), slices.Contains(chosen, name)
					var err error
					switch {
					case !state.Env.Local() || was == now:
						continue
					case now:
						err = m.backend.AttachKey(name, fingerprint)
					default:
						err = m.backend.DetachKey(name, fingerprint)
					}
					if err != nil {
						return "", "", err
					}
					running = running || (state.Known && active(state))
				}
				if running {
					return "saved — reboot the running environments to apply", "", nil
				}
				return "saved", "", nil
			})
		}})
	}}
}

// askName asks for the name of an environment.
func (m *Model) askName(title, prompt, initial string, then func(m *Model, name string) tea.Cmd) *input {
	return &input{name: title, prompt: []string{prompt}, value: []rune(initial),
		submit: func(m *Model, value string) (tea.Cmd, error) {
			name := strings.TrimSpace(value)
			if err := config.ValidateName(name); err != nil {
				return nil, err
			}
			for _, state := range m.snap.Envs {
				if state.Env.Name == name {
					return nil, fmt.Errorf("an environment named %q already exists", name)
				}
			}
			return then(m, name), nil
		}}
}

// create makes an environment and moves the cursor onto it.
func (m *Model) create(name, from string) tea.Cmd {
	return m.doThen("Creating "+name, false, func(io.Writer) (string, string, error) {
		if err := m.backend.Create(name, from); err != nil {
			return "", "", err
		}
		snap, err := m.backend.Load(m.ctx)
		if err != nil {
			return fmt.Sprintf("created %q", name), "", nil
		}
		for _, state := range snap.Envs {
			if state.Env.Name == name {
				return fmt.Sprintf("created %q — Start it from its menu", name), "env:" + state.Env.ID, nil
			}
		}
		return fmt.Sprintf("created %q", name), "", nil
	})
}

func (m *Model) askRename(name string) *input {
	return m.askName("Rename "+name, "The new name (the ssh alias follows; the data stays where it is):", name, func(m *Model, newName string) tea.Cmd {
		return m.do("Renaming", fmt.Sprintf("renamed %q to %q", name, newName), false, func(io.Writer) error {
			return m.backend.Rename(name, newName)
		})
	})
}

// askDestroy asks for the name to be typed: destroying cannot be undone.
func (m *Model) askDestroy(name string, local bool) tea.Cmd {
	what := "its containers, network, volumes (workspace, home, nix, ssh host keys, tokens) and settings"
	if !local {
		what = "its settings"
	}
	m.push(&input{name: "Destroy " + name,
		prompt: []string{paint(red, fmt.Sprintf("This removes %q for good: %s.", name, what)), "Type its name to confirm:"},
		submit: func(m *Model, value string) (tea.Cmd, error) {
			if strings.TrimSpace(value) != name {
				return nil, errors.New("that is not its name — nothing was removed")
			}
			return m.do("Destroying "+name, fmt.Sprintf("destroyed %q", name), false, func(io.Writer) error {
				return m.backend.Destroy(m.ctx, name)
			}), nil
		}})
	return nil
}

// askRemote registers a remote environment: its address, then its name here.
func (m *Model) askRemote() *input {
	return &input{name: "Add remote environment", prompt: []string{"Its address, as " + config.RemoteAddressHint + ".", "On its own machine: Settings → SSH → Copy address."},
		submit: func(m *Model, value string) (tea.Cmd, error) {
			address, err := config.ParseRemoteAddress(value)
			if err != nil {
				return nil, err
			}
			m.pop()
			m.push(m.askName("Add remote environment", fmt.Sprintf("Its name on this machine (it is %q over there):", address.Name), address.Name, func(m *Model, name string) tea.Cmd {
				return m.doThen("Registering "+name, false, func(io.Writer) (string, string, error) {
					if err := m.backend.AddRemote(name, address.Host, address.Port); err != nil {
						return "", "", err
					}
					text := fmt.Sprintf("registered %q — its machine has to let this machine's key in (System → Machine key)", name)
					if snap, err := m.backend.Load(m.ctx); err == nil {
						for _, state := range snap.Envs {
							if state.Env.Name == name {
								return text, "env:" + state.Env.ID, nil
							}
						}
					}
					return text, "", nil
				})
			}))
			return nil, nil
		}}
}

// askKey registers a public key: the pasted line, then a name for it.
func (m *Model) askKey() *input {
	return &input{name: "Register key", prompt: []string{"Paste the public key (one line, like ssh-ed25519 AAAA... comment):"},
		submit: func(m *Model, value string) (tea.Cmd, error) {
			line, err := config.ParsePublicKey(value)
			if err != nil {
				return nil, err
			}
			comment := ""
			if fields := strings.Fields(line); len(fields) > 2 {
				comment = strings.Join(fields[2:], " ")
			}
			m.pop()
			m.push(&input{name: "Register key", prompt: []string{"A name for this key:"}, value: []rune(comment),
				submit: func(m *Model, label string) (tea.Cmd, error) {
					if strings.TrimSpace(label) == "" {
						return nil, errors.New("give the key a name")
					}
					if _, err := config.NormalizeLabel(label); err != nil {
						return nil, err
					}
					return m.doThen("Registering", false, func(io.Writer) (string, string, error) {
						entry, err := m.backend.RegisterKey(line, label)
						if err != nil {
							return "", "", err
						}
						return "registered — attach it from an environment's Settings → SSH, or from the key's menu", "key:" + entry.Fingerprint, nil
					}), nil
				}})
			return nil, nil
		}}
}

// edit opens a file in the user's editor.
func (m *Model) edit(what, path string, then func(m *Model)) tea.Cmd {
	command, err := m.backend.Editor(path)
	return m.shellOut(what, command, err, nil, then)
}

// showMachineKey leaves the full screen, prints the key on the plain
// terminal — where it can be selected — and sends it to the clipboard as
// well. The clipboard goes through the terminal (OSC 52), so over ssh it is
// the clipboard of the machine in front of the user, not of this one.
func (m *Model) showMachineKey() tea.Cmd {
	key, err := m.backend.MachineKey()
	if err != nil {
		return m.fail(err)
	}
	return tea.Batch(tea.SetClipboard(key), tea.Exec(&printout{text: "This machine's public key (also sent to the clipboard, if the terminal allows):\n\n" + key + "\n\nRegister it on another machine with: bh key add\nPress Enter to return."},
		func(err error) tea.Msg { return execDoneMsg{} }))
}

// printout is a "program" that shows a text on the plain terminal and waits
// for Enter.
type printout struct {
	text string
	in   io.Reader
	out  io.Writer
}

func (p *printout) SetStdin(r io.Reader)  { p.in = r }
func (p *printout) SetStdout(w io.Writer) { p.out = w }
func (p *printout) SetStderr(io.Writer)   {}

func (p *printout) Run() error {
	fmt.Fprintln(p.out, p.text)
	waitForEnter(p.in)
	return nil
}

// rollbackMenu lists the environment's snapshots, newest first. Choosing
// one asks, and then puts its data back; what is there now is kept in a
// snapshot of its own first.
func (m *Model) rollbackMenu(name string) *menu {
	entries, err := m.backend.Snapshots(name)
	note := []string{"Put the workspace and the home directory back to a snapshot. What is there now is kept in a snapshot first."}
	switch {
	case err != nil:
		note = []string{err.Error()}
	case len(entries) == 0:
		note = []string{fmt.Sprintf("%q has no snapshots yet — Maintenance → Snapshot takes one.", name)}
	}
	return &menu{name: "Roll back " + name, note: note, build: func(m *Model) []item {
		items := make([]item, 0, len(entries))
		for _, entry := range entries {
			items = append(items, item{label: entry.About, run: func(m *Model) tea.Cmd {
				m.push(&confirm{name: "Roll back " + name, color: yellow,
					text: []string{"Replace the workspace and the home directory with this snapshot?", entry.About, "What is there now is kept in a snapshot first."},
					onYes: func(m *Model) tea.Cmd {
						return m.doThen("Rolling "+name+" back", false, func(progress io.Writer) (string, string, error) {
							kept, err := m.backend.Rollback(m.ctx, name, entry.Name, progress)
							return fmt.Sprintf("%q is rolled back — how it was is in the snapshot %s", name, kept), "", err
						})
					}})
				return nil
			}})
		}
		return items
	}}
}

// resetMenu offers the two steps of a reset, the one that loses nothing of
// the user's first. Each asks before it does anything.
func (m *Model) resetMenu(name string) *menu {
	ask := func(home bool, tone color, text ...string) func(m *Model) tea.Cmd {
		return func(m *Model) tea.Cmd {
			m.push(&confirm{name: "Reset " + name, color: tone, text: text,
				onYes: func(m *Model) tea.Cmd {
					return m.doThen("Resetting "+name, false, func(progress io.Writer) (string, string, error) {
						text, err := m.backend.Reset(m.ctx, name, home, progress)
						return text, "", err
					})
				}})
			return nil
		}
	}
	return &menu{name: "Reset " + name,
		note: []string{"Take part of the environment back to what the image provides. The workspace is never touched."},
		build: func(*Model) []item {
			return []item{
				{label: "Tools", value: "nix store and caches", color: yellow, run: ask(false, yellow,
					"Remove the nix store and the caches, and install the tools again?",
					"The environment is stopped. The workspace and what is in the home directory stay; at the next Start init.sh runs again.")},
				{label: "Tools and home", value: "everything but the workspace", color: red, run: ask(true, red,
					"Delete the home directory and the nix store, and start over from the image?",
					"The environment is stopped. The workspace is kept. Logins, shell history and settings in the home directory are gone — a snapshot of them is taken first.")},
			}
		}}
}
