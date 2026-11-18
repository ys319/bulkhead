package main

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/engine/enginetest"
	"github.com/ys319/bulkhead/internal/filemode"
	"github.com/ys319/bulkhead/internal/plan"
)

// doctorBench is a bench on a machine with the programs doctor looks for,
// and the environment "web" up: nothing for doctor to complain about.
func doctorBench(t *testing.T) (b *bench, holder, dev string) {
	t.Helper()
	b = newBench(t)
	b.path["ssh"], b.path["docker"] = true, true
	b.ok("create", "web")
	b.ok("up", "web")
	id := b.env("web").ID
	return b, plan.HolderName(id), plan.DevName(id)
}

// marked returns the lines of a doctor output that carry the mark.
func marked(output, mark string) []string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, mark) {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestDoctorPassesWhatBhCreated(t *testing.T) {
	b, _, _ := doctorBench(t)
	out := b.ok("doctor")

	// Not one warning either: the containers are compared with what bh up
	// just created them from, so any line here is doctor and up disagreeing
	// about the plan.
	if bad := append(marked(out, " FAIL "), marked(out, " warn ")...); len(bad) > 0 {
		t.Errorf("doctor complains about a fresh environment:\n%s", strings.Join(bad, "\n"))
	}
	// Where permission bits are not kept there is no mode to report.
	key := "  ok   machine key is private (0600)"
	if !filemode.Kept {
		key = "  ok   machine key is in place"
	}
	for _, part := range []string{
		"== machine ==\n", "== web ==\n", "doctor: no failures\n",
		"  ok   Docker Engine 29.0.0",
		key,
		"  ok   ssh configuration includes bh's Host entries",
		"  ok   holder is healthy: the egress filter passed its self-check",
		"  ok   dev is in the holder's network namespace",
		"  ok   holder has no capability but NET_ADMIN",
		"  ok   ssh is published on 127.0.0.1:22001, as the settings say",
		"  ok   holder mounts nothing",
		"  ok   dev has no capabilities (cap_drop ALL)",
		"  ok   dev cannot gain privileges (no-new-privileges)",
		"  ok   dev publishes no ports",
		"  ok   dev mounts only the environment's volumes and init.sh",
	} {
		if !strings.Contains(out, part) {
			t.Errorf("doctor lacks %q:\n%s", part, out)
		}
	}
}

// What doctor is for: a container that has the right name and not the
// boundary — recreated by hand with the docker CLI, or created by a bh
// with a bug. Each case breaks one thing on the containers as the engine
// reports them.
func TestDoctorFailsABrokenBoundary(t *testing.T) {
	tests := []struct {
		name   string
		tamper func(engine *enginetest.Fake, holder, dev string)
		want   string
	}{
		{"privileged dev", func(e *enginetest.Fake, _, dev string) {
			e.Created[dev].Host.Privileged = true
		}, "dev is privileged"},
		{"a capability on dev", func(e *enginetest.Fake, _, dev string) {
			e.Created[dev].Host.CapAdd = []string{"SYS_ADMIN"}
		}, "dev has capabilities bh does not give it: SYS_ADMIN"},
		{"dev keeps the default capabilities", func(e *enginetest.Fake, _, dev string) {
			e.Created[dev].Host.CapDrop = nil
		}, "dev does not drop all capabilities"},
		{"no no-new-privileges", func(e *enginetest.Fake, _, dev string) {
			e.Created[dev].Host.SecurityOpt = nil
		}, "dev lacks no-new-privileges"},
		{"seccomp turned off", func(e *enginetest.Fake, _, dev string) {
			e.Created[dev].Host.SecurityOpt = append(e.Created[dev].Host.SecurityOpt, "seccomp=unconfined")
		}, "dev has a confinement of the engine turned off: seccomp=unconfined"},
		{"a host directory bound with -v", func(e *enginetest.Fake, _, dev string) {
			e.Created[dev].Host.Binds = []string{"/home/me:/host"}
		}, "dev mounts /home/me of the host at /host"},
		{"a host directory given as a mount", func(e *enginetest.Fake, _, dev string) {
			host := e.Created[dev].Host
			host.Mounts = append(host.Mounts, mount.Mount{Type: mount.TypeBind, Source: "/", Target: "/host", ReadOnly: true})
		}, "dev mounts / of the host at /host"},
		{"the Docker socket without the opt-in", func(e *enginetest.Fake, _, dev string) {
			host := e.Created[dev].Host
			host.Mounts = append(host.Mounts, mount.Mount{Type: mount.TypeBind, Source: plan.DockerSocketPath, Target: plan.DockerSocketPath})
		}, "the Docker socket is mounted into dev"},
		{"a writable init.sh", func(e *enginetest.Fake, _, dev string) {
			mounts := e.Created[dev].Host.Mounts
			for i := range mounts {
				if mounts[i].Target == plan.InitScriptTarget {
					mounts[i].ReadOnly = false
				}
			}
		}, "dev can write to /etc/bulkhead/init.sh"},
		{"a port published on dev", func(e *enginetest.Fake, _, dev string) {
			e.Created[dev].Host.PortBindings = network.PortMap{
				network.MustParsePort("8080/tcp"): {{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: "8080"}},
			}
		}, "dev publishes ports itself: 8080/tcp on 0.0.0.0:8080"},
		{"dev in the host's process namespace", func(e *enginetest.Fake, _, dev string) {
			e.Created[dev].Host.PidMode = "host"
		}, "dev shares a process namespace that is not its own (pid: host)"},
		{"dev on another network", func(e *enginetest.Fake, _, dev string) {
			e.Containers[dev].NetworkMode = "bridge"
		}, "dev is not in the holder's network namespace (network mode: bridge)"},
		{"ssh on every interface without publish", func(e *enginetest.Fake, holder, _ string) {
			bindings := e.Created[holder].Host.PortBindings[network.MustParsePort("22/tcp")]
			bindings[0].HostIP = netip.MustParseAddr("0.0.0.0")
		}, "ssh is published on 0.0.0.0:22001; the settings say 127.0.0.1:22001"},
		{"ssh published without an address", func(e *enginetest.Fake, holder, _ string) {
			bindings := e.Created[holder].Host.PortBindings[network.MustParsePort("22/tcp")]
			bindings[0].HostIP = netip.Addr{}
		}, "ssh is published on 0.0.0.0:22001; the settings say 127.0.0.1:22001"},
		{"a privileged holder", func(e *enginetest.Fake, holder, _ string) {
			e.Created[holder].Host.Privileged = true
		}, "holder is privileged"},
		{"a capability on the holder", func(e *enginetest.Fake, holder, _ string) {
			e.Created[holder].Host.CapAdd = []string{"NET_ADMIN", "cap_sys_module"}
		}, "holder has capabilities bh does not give it: SYS_MODULE"},
		{"a holder on the host's network", func(e *enginetest.Fake, holder, _ string) {
			e.Containers[holder].NetworkMode = "host"
		}, `holder is on the network "host"`},
		{"a healthcheck that always passes", func(e *enginetest.Fake, holder, _ string) {
			e.Created[holder].Config.Healthcheck = &container.HealthConfig{Test: []string{"CMD", "true"}}
		}, "holder's healthcheck is not the one bh sets (CMD true)"},
		{"IPv6 left on", func(e *enginetest.Fake, holder, _ string) {
			delete(e.Created[holder].Host.Sysctls, "net.ipv6.conf.all.disable_ipv6")
		}, "holder does not set the sysctl net.ipv6.conf.all.disable_ipv6"},
		{"the Docker socket in the holder", func(e *enginetest.Fake, holder, _ string) {
			e.Created[holder].Host.Binds = []string{"/var/run/docker.sock:/var/run/docker.sock"}
		}, "the Docker socket is mounted into holder"},
		{"an unhealthy holder", func(e *enginetest.Fake, holder, _ string) {
			e.HealthAfterStart = []string{"unhealthy"}
			e.Containers[holder].Health = "unhealthy"
		}, "holder is unhealthy"},
		{"dev down", func(e *enginetest.Fake, _, dev string) {
			e.Containers[dev].Status = "exited"
		}, "dev is not running while the holder is (bh up web starts it)"},
		{"the holder gone", func(e *enginetest.Fake, holder, _ string) {
			delete(e.Containers, holder)
		}, "the holder container is missing while dev runs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, holder, dev := doctorBench(t)
			tt.tamper(b.engine, holder, dev)

			code, out, _ := b.run("doctor", "web")
			if code != 1 {
				t.Errorf("exit %d, want 1\n%s", code, out)
			}
			failed := strings.Join(marked(out, " FAIL "), "\n")
			if !strings.Contains(failed, tt.want) {
				t.Errorf("no failure mentions %q:\n%s", tt.want, out)
			}
			if !strings.Contains(out, "failure(s)\n") {
				t.Errorf("the summary does not count the failure:\n%s", out)
			}
		})
	}
}

// Warnings are shown and leave the exit status alone.
func TestDoctorWarnsWithoutFailing(t *testing.T) {
	t.Run("an image of another bh", func(t *testing.T) {
		b, _, dev := doctorBench(t)
		b.engine.Created[dev].Config.Image = "bulkhead-dev-base:000000000000"
		out := b.ok("doctor", "web")
		if !strings.Contains(out, " warn  dev runs the image bulkhead-dev-base:000000000000") || !strings.Contains(out, "bh restart web recreates it") {
			t.Errorf("output:\n%s", out)
		}
	})

	t.Run("settings edited under a running environment", func(t *testing.T) {
		b, _, _ := doctorBench(t)
		web := b.env("web")
		web.Restart = "no"
		if err := (config.Store{Dir: b.dir}).SaveEnvironment(web); err != nil {
			t.Fatal(err)
		}
		out := b.ok("doctor", "web")
		for _, part := range []string{
			` warn  holder has the restart policy "unless-stopped"; the settings say "no"`,
			" warn  dev was created from other settings than the current ones — the next bh up web recreates it",
		} {
			if !strings.Contains(out, part) {
				t.Errorf("doctor lacks %q:\n%s", part, out)
			}
		}
	})

	t.Run("a volume from elsewhere", func(t *testing.T) {
		b, _, dev := doctorBench(t)
		b.engine.Created[dev].Host.Binds = []string{"scratch:/scratch"}
		if out := b.ok("doctor", "web"); !strings.Contains(out, " warn  dev mounts the volume scratch at /scratch") {
			t.Errorf("output:\n%s", out)
		}
	})

	t.Run("an extra port on the holder", func(t *testing.T) {
		b, holder, _ := doctorBench(t)
		b.engine.Created[holder].Host.PortBindings[network.MustParsePort("8080/tcp")] = []network.PortBinding{
			{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "8080"},
		}
		if out := b.ok("doctor", "web"); !strings.Contains(out, " warn  holder publishes ports the settings do not ask for: 8080/tcp on 127.0.0.1:8080") {
			t.Errorf("output:\n%s", out)
		}
	})
}

// The opt-ins that give up part of the boundary are said every time, from
// the settings: before the environment exists, and when all is as planned.
func TestDoctorSaysTheOptInsEveryTime(t *testing.T) {
	b := newBench(t)
	b.path["ssh"], b.path["docker"] = true, true
	b.ok("create", "web", "--publish")
	web := b.env("web")
	web.DockerSocket = true
	if err := (config.Store{Dir: b.dir}).SaveEnvironment(web); err != nil {
		t.Fatal(err)
	}

	check := func(out string) {
		t.Helper()
		for _, part := range []string{
			" warn  dev gets the Docker socket by opt-in (dockerSocket: true) — this environment is not isolated from the host",
			" warn  ssh is published beyond this machine (publish: true, port 22001)",
		} {
			if !strings.Contains(out, part) {
				t.Errorf("doctor lacks %q:\n%s", part, out)
			}
		}
	}
	check(b.ok("doctor"))

	b.ok("up", "web")
	out := b.ok("doctor")
	check(out)
	// The socket and its group are part of the plan now: nothing else to say.
	if bad := append(marked(out, " FAIL "), marked(out, " warn ")...); len(bad) != 2 {
		t.Errorf("more than the two standing warnings:\n%s", out)
	}
	for _, part := range []string{
		"  ok   dev mounts the environment's volumes, init.sh and (by opt-in) the Docker socket",
		"  ok   ssh is published on 0.0.0.0:22001, as the settings say",
	} {
		if !strings.Contains(out, part) {
			t.Errorf("doctor lacks %q:\n%s", part, out)
		}
	}

	// The opt-in taken back in the settings while the container still has
	// the socket: the boundary the settings promise is not there.
	web.DockerSocket = false
	if err := (config.Store{Dir: b.dir}).SaveEnvironment(web); err != nil {
		t.Fatal(err)
	}
	code, out, _ := b.run("doctor")
	if code != 1 || !strings.Contains(out, " FAIL  the Docker socket is mounted into dev") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// Stopping an environment to change a setting is the normal way to change
// it. The containers left behind are replaced by the next up, so they are
// not held against the new settings.
func TestDoctorDoesNotHoldStoppedContainersAgainstNewSettings(t *testing.T) {
	b, _, dev := doctorBench(t)
	b.ok("down", "web")
	out := b.ok("doctor", "web")
	// Unchanged settings: up would start these very containers, so they
	// are checked.
	if !strings.Contains(out, " skip  not running") || !strings.Contains(out, "  ok   dev has no capabilities") {
		t.Errorf("stopped, unchanged:\n%s", out)
	}

	b.ok("configure", "web", "--port", "23000", "--publish", "on")
	out = b.ok("doctor", "web")
	if !strings.Contains(out, " skip  the stopped containers are not checked") || len(marked(out, " FAIL ")) > 0 {
		t.Errorf("stopped, settings changed:\n%s", out)
	}

	// A stopped container that up would keep is still checked.
	b.ok("configure", "web", "--port", "22001", "--publish", "off")
	b.engine.Created[dev].Host.Privileged = true
	code, out, _ := b.run("doctor", "web")
	if code != 1 || !strings.Contains(out, " FAIL  dev is privileged") {
		t.Errorf("stopped, tampered: exit %d\n%s", code, out)
	}
}

func TestDoctorSections(t *testing.T) {
	b := newBench(t)
	b.path["ssh"], b.path["docker"] = true, true
	b.ok("create", "web")
	b.ok("create", "api")
	b.ok("remote", "add", "example.test:22/box")

	out := b.ok("doctor")
	for _, part := range []string{
		"== machine ==", "== api ==", "== box ==", "== web ==",
		" skip  not created yet (bh up web creates and starts it)",
		" skip  remote environment — run bh doctor on the machine it lives on",
		// Nothing was started yet, so nothing was made; that is not a failure.
		" warn  no machine key yet",
		" warn  no init.sh yet",
	} {
		if !strings.Contains(out, part) {
			t.Errorf("doctor lacks %q:\n%s", part, out)
		}
	}

	// One environment: the machine and that one.
	out = b.ok("doctor", "api")
	if !strings.Contains(out, "== machine ==") || !strings.Contains(out, "== api ==") || strings.Contains(out, "== web ==") {
		t.Errorf("doctor api:\n%s", out)
	}

	if stderr := b.fails(1, "doctor", "nope"); !strings.Contains(stderr, `no environment named "nope"`) {
		t.Errorf("stderr: %s", stderr)
	}
	if stderr := b.fails(2, "doctor", "web", "api"); !strings.Contains(stderr, "usage: bh doctor") {
		t.Errorf("stderr: %s", stderr)
	}
	if stderr := b.fails(2, "doctor", "--fix"); !strings.Contains(stderr, "not defined") {
		t.Errorf("stderr: %s", stderr)
	}
}

// Doctor looks and touches nothing: unlike up it makes no key, no init.sh,
// and asks the engine for nothing but inspections.
func TestDoctorOnlyReads(t *testing.T) {
	b := newBench(t)
	b.path["ssh"], b.path["docker"] = true, true
	b.ok("create", "web")
	b.ok("create", "gpu")
	gpu := b.env("gpu")
	gpu.DockerSocket, gpu.GPU, gpu.CDIDevices = true, "cdi", []string{"nvidia.com/gpu=0"}
	if err := (config.Store{Dir: b.dir}).SaveEnvironment(gpu); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(b.dir)
	if err != nil {
		t.Fatal(err)
	}

	b.run("doctor")
	after, err := os.ReadDir(b.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("doctor changed the configuration directory: %v -> %v", before, after)
	}
	if len(b.engine.Calls) != 0 {
		t.Errorf("doctor did something on the engine: %v", b.engine.Calls)
	}

	// Also with containers there: no probe for the socket's or the
	// devices' groups, which would run a container.
	b.engine.Devices = []string{"nvidia.com/gpu=0"}
	b.engine.DeviceGroups = []int{44}
	b.ok("up", "gpu")
	b.engine.Calls = nil
	out := b.ok("doctor", "gpu")
	if len(b.engine.Calls) != 0 {
		t.Errorf("doctor did something on the engine: %v", b.engine.Calls)
	}
	if !strings.Contains(out, "  ok   dev has the GPU request of the settings (cdi: nvidia.com/gpu=0)") || strings.Contains(out, "other settings") {
		t.Errorf("output:\n%s", out)
	}
}

func TestDoctorWithoutDocker(t *testing.T) {
	for name, down := range map[string]func(b *bench){
		"no connection": func(b *bench) { b.dialErr = errors.New("cannot connect") },
		"no answer":     func(b *bench) { b.engine.InfoErr = errors.New("cannot connect") },
	} {
		t.Run(name, func(t *testing.T) {
			b, _, _ := doctorBench(t)
			down(b)
			code, out, _ := b.run("doctor")
			// One failure, on the machine; the environment is not blamed
			// for what could not be seen.
			if code != 1 || !strings.Contains(out, " FAIL  the Docker Engine does not answer: cannot connect") ||
				!strings.Contains(out, " skip  containers not checked") || !strings.Contains(out, "doctor: 1 failure(s)") {
				t.Errorf("exit %d\n%s", code, out)
			}
		})
	}
}

func TestDoctorChecksTheMachine(t *testing.T) {
	tests := []struct {
		name   string
		damage func(t *testing.T, b *bench)
		code   int
		want   string
	}{
		{"no ssh client", func(_ *testing.T, b *bench) { delete(b.path, "ssh") }, 1,
			" FAIL  ssh is not in PATH"},
		{"no docker CLI", func(_ *testing.T, b *bench) { delete(b.path, "docker") }, 0,
			" warn  docker is not in PATH"},
		{"no VS Code", func(_ *testing.T, b *bench) { delete(b.path, "code") }, 0,
			" warn  code is not in PATH"},
		{"a machine key others can read", func(t *testing.T, b *bench) {
			if err := os.Chmod(filepath.Join(b.dir, "id_ed25519"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, 1, " FAIL  the machine key is accessible to other users (0644) — chmod 600 "},
		{"a damaged key registry", func(t *testing.T, b *bench) {
			if err := os.WriteFile(filepath.Join(b.dir, "keys.json"), []byte("{"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, 1, " FAIL  the key registry cannot be read"},
		{"a key that is not registered", func(t *testing.T, b *bench) {
			web := b.env("web")
			web.AuthorizedKeys = []string{"SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
			if err := (config.Store{Dir: b.dir}).SaveEnvironment(web); err != nil {
				t.Fatal(err)
			}
		}, 1, ` FAIL  environment "web" refers to key SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA, which is not registered`},
		{"init.sh the container cannot read", func(t *testing.T, b *bench) {
			if err := os.Chmod(filepath.Join(b.dir, "init.sh"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, 0, " warn  init.sh is not readable by others (0700)"},
		{"no Include line", func(t *testing.T, b *bench) {
			if err := os.Remove(filepath.Join(b.home, ".ssh", "config")); err != nil {
				t.Fatal(err)
			}
		}, 0, " warn  ssh configuration does not include bh's Host entries yet"},
		{"damaged markers", func(t *testing.T, b *bench) {
			if err := os.WriteFile(filepath.Join(b.home, ".ssh", "config"), []byte("# >>> bh >>>\nInclude bh/config\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, 1, " FAIL  the bh markers in "},
		{"Host entries of before a rename", func(t *testing.T, b *bench) {
			web := b.env("web")
			web.Name = "site"
			if err := (config.Store{Dir: b.dir}).SaveEnvironment(web); err != nil {
				t.Fatal(err)
			}
		}, 0, " warn  the Host entries are out of date"},
		{"an environment that cannot be read", func(t *testing.T, b *bench) {
			if err := os.MkdirAll(filepath.Join(b.dir, "environments", "aaaaaaaaaaaa"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(b.dir, "environments", "aaaaaaaaaaaa", "env.json"), []byte("{"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, 1, "== aaaaaaaaaaaa ==\n FAIL  "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !filemode.Kept && strings.Contains(tt.want, " (0") {
				t.Skip("permission bits are not kept on this system")
			}
			b, _, _ := doctorBench(t)
			tt.damage(t, b)
			code, out, _ := b.run("doctor")
			if code != tt.code || !strings.Contains(out, tt.want) {
				t.Errorf("exit %d, want %d and %q:\n%s", code, tt.code, tt.want, out)
			}
		})
	}
}
