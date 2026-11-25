package doctor

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/ys319/bulkhead/internal/engine"
)

// ready is a machine with nothing to complain about.
func ready() Machine {
	return Machine{
		Engine:    &engine.Info{Version: "29.0.0", APIVersion: "1.52", OS: "linux", Arch: "arm64"},
		DockerCLI: true, SSH: true, VSCode: true,
		MachineKey:   File{Path: "/cfg/id_ed25519", Mode: 0o600},
		InitScript:   File{Path: "/cfg/init.sh", Mode: 0o755},
		Keys:         2,
		ShmSize:      "8g",
		Environments: 1,
		SSHConfig:    "/home/me/.ssh/config",
		Included:     true, EntriesCurrent: true,
	}
}

func TestAReadyMachinePasses(t *testing.T) {
	findings := CheckMachine(ready())
	for _, f := range findings {
		if f.Level != OK {
			t.Errorf("%s: %s", f.Level, f.Message)
		}
	}
	for _, part := range []string{"Docker Engine 29.0.0 (API 1.52, linux/arm64)", "key registry: 2 key(s)",
		"an environment's /dev/shm holds up to 8g (bh settings)"} {
		if !has(findings, OK, part) {
			t.Errorf("no ok finding with %q:\n%s", part, listing(findings))
		}
	}
}

func TestMachineChecks(t *testing.T) {
	missing := File{Path: "/cfg/x", Err: fs.ErrNotExist}
	tests := []struct {
		name   string
		change func(m *Machine)
		level  Level
		want   string
	}{
		{"engine down", func(m *Machine) {
			m.Engine, m.EngineErr = nil, errors.New("connection refused")
		}, Fail, "the Docker Engine does not answer: connection refused"},
		{"no docker CLI", func(m *Machine) { m.DockerCLI = false }, Warn, "docker is not in PATH"},
		{"no ssh", func(m *Machine) { m.SSH = false }, Fail, "ssh is not in PATH"},
		{"no code", func(m *Machine) { m.VSCode = false }, Warn, "code is not in PATH"},

		{"no machine key yet", func(m *Machine) { m.MachineKey = missing }, Warn, "no machine key yet"},
		{"a key the group can read", func(m *Machine) { m.MachineKey.Mode = 0o640 }, Fail,
			"the machine key is accessible to other users (0640) — chmod 600 /cfg/id_ed25519"},
		// Windows has no mode bits; what Go reports there is not a finding.
		{"a key on Windows", func(m *Machine) { m.MachineKey.NoPermissions, m.MachineKey.Mode = true, 0o666 }, OK, "machine key is in place"},
		{"a key that cannot be looked at", func(m *Machine) {
			m.MachineKey = File{Path: "/cfg/id_ed25519", Err: fs.ErrPermission}
		}, Fail, "the machine key cannot be looked at"},

		{"a registry that cannot be read", func(m *Machine) {
			m.KeysErr = errors.New("/cfg/keys.json: unexpected end of JSON input")
		}, Fail, "the key registry cannot be read — /cfg/keys.json"},
		{"an empty registry", func(m *Machine) { m.Keys = 0 }, OK, "key registry: no keys besides the machine key"},

		{"settings that cannot be read", func(m *Machine) { m.SettingsErr = errors.New("/cfg/settings.json: no version") }, Fail,
			"this machine's settings cannot be read, and no environment starts without them — /cfg/settings.json: no version"},

		{"no init.sh yet", func(m *Machine) { m.InitScript = missing }, Warn, "no init.sh yet"},
		// What the engine of the TypeScript version made of a missing file.
		{"init.sh a directory", func(m *Machine) { m.InitScript.Mode = fs.ModeDir | 0o755 }, Fail, "/cfg/init.sh is not a file"},
		{"init.sh closed to others", func(m *Machine) { m.InitScript.Mode = 0o700 }, Warn,
			"init.sh is not readable by others (0700)"},
		{"init.sh on Windows", func(m *Machine) { m.InitScript.NoPermissions, m.InitScript.Mode = true, 0o600 }, OK, "init.sh is in place"},

		{"no environments", func(m *Machine) {
			m.Environments, m.Included, m.EntriesCurrent = 0, false, false
		}, Skip, "no environments — the ssh configuration is not checked"},
		{"no Include line", func(m *Machine) { m.Included = false }, Warn, "does not include bh's Host entries yet"},
		{"damaged markers", func(m *Machine) { m.Included, m.IncludeBroken = false, true }, Fail,
			"the bh markers in /home/me/.ssh/config are broken"},
		{"stale Host entries", func(m *Machine) { m.EntriesCurrent = false }, Warn, "the Host entries are out of date"},
		{"an unreadable ssh configuration", func(m *Machine) { m.SSHConfigErr = fs.ErrPermission }, Fail,
			"the ssh configuration cannot be read"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			machine := ready()
			tt.change(&machine)
			findings := CheckMachine(machine)
			if !has(findings, tt.level, tt.want) {
				t.Errorf("no %s finding with %q:\n%s", tt.level, tt.want, listing(findings))
			}
			// One thing wrong is one finding.
			if tt.level == OK {
				return
			}
			others := 0
			for _, f := range findings {
				if f.Level != OK {
					others++
				}
			}
			if others != 1 {
				t.Errorf("%d findings that are not ok, want 1:\n%s", others, listing(findings))
			}
		})
	}
}
