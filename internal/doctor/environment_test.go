package doctor

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ys319/bulkhead/internal/engine"
	"github.com/ys319/bulkhead/internal/ops"
	"github.com/ys319/bulkhead/internal/plan"
)

var (
	testEnv = plan.Env{
		ID: "3f9a2c1e7b04", Name: "main", SSHPort: 22001, Restart: "unless-stopped",
		AuthorizedKeys: []string{"ssh-ed25519 AAAA one"},
		InitScript:     "/home/me/.config/bh/init.sh",
	}
	testTarget = plan.Target{Owner: "cfg-1", HolderImage: "bulkhead-holder:abc", DevImage: "bulkhead-dev-base:def"}
)

// created is the environment as bh up leaves it: both containers made from
// the plan and running, the holder healthy. realized is the plan with what
// the probes on the engine told (group ids); the settings handed to the
// checks do not have that.
func created(settings, realized plan.Env) Environment {
	holderSpec := plan.Holder(realized, testTarget)
	holder := engine.DescribeSpec(holderSpec.Name, holderSpec.Config, holderSpec.Host)
	holder.ID, holder.Status, holder.Health, holder.StartedAt = "holder-id", "running", "healthy", time.Unix(1000, 0)

	devSpec := plan.Dev(realized, testTarget, holder.ID)
	dev := engine.DescribeSpec(devSpec.Name, devSpec.Config, devSpec.Host)
	dev.ID, dev.Status, dev.StartedAt = "dev-id", "running", time.Unix(1001, 0)

	return Environment{Name: settings.Name, Plan: settings, Target: testTarget, Holder: &holder, Dev: &dev}
}

func has(findings []Finding, level Level, part string) bool {
	return slices.ContainsFunc(findings, func(f Finding) bool {
		return f.Level == level && strings.Contains(f.Message, part)
	})
}

func listing(findings []Finding) string {
	var b strings.Builder
	for _, f := range findings {
		fmt.Fprintf(&b, "  %-4s %s\n", f.Level, f.Message)
	}
	return b.String()
}

func TestWhatBhCreatesPasses(t *testing.T) {
	publish, socket, legacy, cdi := testEnv, testEnv, testEnv, testEnv
	publish.Publish = true
	socket.DockerSocket = true
	legacy.GPU = "legacy"
	cdi.GPU, cdi.CDIDevices, cdi.DockerSocket = "cdi", []string{"nvidia.com/gpu=0", "nvidia.com/gpu=1"}, true
	// What realize adds from its probes.
	socketRealized, cdiRealized := socket, cdi
	socketRealized.SocketGID = 998
	cdiRealized.SocketGID, cdiRealized.DeviceGIDs = 998, []int{44, 109}

	tests := []struct {
		name               string
		settings, realized plan.Env
		standing           int
	}{
		{"defaults", testEnv, testEnv, 0},
		{"publish", publish, publish, 1},
		{"docker socket", socket, socketRealized, 1},
		{"gpu legacy", legacy, legacy, 0},
		{"gpu cdi with the socket", cdi, cdiRealized, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := CheckLocal(created(tt.settings, tt.realized))
			standing := 0
			for _, f := range findings {
				switch {
				case f.Standing && f.Level == Warn:
					standing++
				case f.Level != OK:
					t.Errorf("%s: %s", f.Level, f.Message)
				}
			}
			if standing != tt.standing {
				t.Errorf("%d standing warnings, want %d:\n%s", standing, tt.standing, listing(findings))
			}
			if want := OK; Worst(findings) != want {
				t.Errorf("Worst = %s, want %s", Worst(findings), want)
			}
		})
	}
}

func TestContainerChecks(t *testing.T) {
	socket, legacy, cdi := testEnv, testEnv, testEnv
	socket.DockerSocket, socket.SocketGID = true, 998
	legacy.GPU = "legacy"
	cdi.GPU, cdi.CDIDevices = "cdi", []string{"a", "b"}

	gpuRequest := engine.DeviceRequest{Count: -1, Capabilities: [][]string{{"gpu"}}}
	devMount := func(in *Environment, target string) *engine.Mount {
		for i := range in.Dev.Mounts {
			if in.Dev.Mounts[i].Target == target {
				return &in.Dev.Mounts[i]
			}
		}
		panic("no mount at " + target)
	}

	tests := []struct {
		name   string
		env    plan.Env
		change func(in *Environment)
		level  Level
		want   string
		// only: the finding is the only one that is not ok.
		only bool
	}{
		// What the engine reports in another spelling is the same thing.
		{"capabilities in the engine's other spellings", testEnv, func(in *Environment) {
			in.Holder.CapAdd, in.Holder.CapDrop, in.Dev.CapDrop = []string{"cap_net_admin"}, []string{"all"}, []string{"CAP_ALL"}
		}, OK, "holder has no capability but NET_ADMIN", true},
		{"no-new-privileges without a value", testEnv, func(in *Environment) {
			in.Dev.SecurityOpt = []string{"no-new-privileges"}
		}, OK, "dev cannot gain privileges", true},
		{"no-new-privileges with =", testEnv, func(in *Environment) {
			in.Dev.SecurityOpt = []string{"no-new-privileges=true"}
		}, OK, "dev cannot gain privileges", true},
		{"init.sh under Docker Desktop's prefix", testEnv, func(in *Environment) {
			devMount(in, plan.InitScriptTarget).Source = "/host_mnt/home/me/.config/bh/init.sh"
		}, OK, "dev mounts only the environment's volumes and init.sh", true},
		// macOS: the configuration directory is under /var/folders, which
		// is a symbolic link; Docker Desktop reports where it leads.
		{"init.sh by its real path", testEnv, func(in *Environment) {
			in.Resolved = map[string]string{"/home/me/.config/bh/init.sh": "/private/home/me/.config/bh/init.sh"}
			devMount(in, plan.InitScriptTarget).Source = "/host_mnt/private/home/me/.config/bh/init.sh"
		}, OK, "dev mounts only the environment's volumes and init.sh", true},
		{"a real path that is not init.sh's", testEnv, func(in *Environment) {
			in.Resolved = map[string]string{"/home/me/.config/bh/init.sh": "/private/home/me/.config/bh/init.sh"}
			devMount(in, plan.InitScriptTarget).Source = "/host_mnt/private/etc/shadow"
		}, Fail, "dev mounts /host_mnt/private/etc/shadow of the host at /etc/bulkhead/init.sh", false},
		{"the socket where Docker Desktop keeps it", socket, func(in *Environment) {
			devMount(in, plan.DockerSocketPath).Source = "/run/host-services/docker.proxy.sock"
		}, OK, "(by opt-in) the Docker socket", false},
		{"a tmpfs", testEnv, func(in *Environment) {
			in.Dev.Mounts = append(in.Dev.Mounts, engine.Mount{Type: "tmpfs", Target: "/tmp"})
		}, OK, "dev mounts only the environment's volumes and init.sh", true},
		{"no restart policy named", func() plan.Env { env := testEnv; env.Restart = "no"; return env }(), func(in *Environment) {
			in.Holder.RestartPolicy, in.Dev.RestartPolicy = "", ""
		}, OK, "dev has no capabilities", true},
		{"cdi devices in another order", cdi, func(in *Environment) {
			in.Dev.DeviceRequests[0].DeviceIDs = []string{"b", "a"}
		}, OK, "dev has the GPU request of the settings (cdi: a, b)", true},
		{"gpus asked for the way the docker CLI does", legacy, func(in *Environment) {
			in.Dev.DeviceRequests = []engine.DeviceRequest{{Driver: "nvidia", Count: -1, Capabilities: [][]string{{"gpu"}}}}
		}, OK, "dev has the GPU request of the settings (gpu)", true},

		// Privileges.
		{"privileged leaves no ok about capabilities", testEnv, func(in *Environment) {
			in.Dev.Privileged = true
		}, Fail, "dev is privileged", false},
		{"every capability", testEnv, func(in *Environment) {
			in.Dev.CapAdd = []string{"ALL"}
		}, Fail, "dev has capabilities bh does not give it: ALL", true},
		{"a holder without NET_ADMIN", testEnv, func(in *Environment) {
			in.Holder.CapAdd = nil
		}, Fail, "holder lacks NET_ADMIN, which bh gives it", true},
		{"no-new-privileges switched off", testEnv, func(in *Environment) {
			in.Dev.SecurityOpt = []string{"no-new-privileges:false"}
		}, Fail, "dev lacks no-new-privileges", true},
		{"apparmor off", testEnv, func(in *Environment) {
			in.Holder.SecurityOpt = []string{"apparmor:unconfined"}
		}, Fail, "holder has a confinement of the engine turned off: apparmor=unconfined", true},
		{"SELinux labels off", testEnv, func(in *Environment) {
			in.Dev.SecurityOpt = append(in.Dev.SecurityOpt, "label=disable")
		}, Fail, "dev has a confinement of the engine turned off: label=disable", true},

		// What of the host the container reaches.
		{"the host's IPC namespace", testEnv, func(in *Environment) {
			in.Dev.IpcMode = "host"
		}, Fail, "dev shares an IPC namespace that is not its own (ipc: host)", true},
		{"the engine's default IPC mode", testEnv, func(in *Environment) {
			in.Dev.IpcMode, in.Holder.IpcMode = "private", "shareable"
		}, OK, "dev has no capabilities", true},
		{"outside the user namespace remapping", testEnv, func(in *Environment) {
			in.Dev.UsernsMode = "host"
		}, Fail, "dev is exempt from the engine's user namespace remapping", true},
		{"a disk of the host", testEnv, func(in *Environment) {
			in.Dev.Devices = []string{"/dev/sda"}
		}, Fail, "dev has device nodes of the host: /dev/sda", true},
		{"the home directory", testEnv, func(in *Environment) {
			in.Dev.Mounts = append(in.Dev.Mounts, engine.Mount{Type: "bind", Source: "/home/me", Target: "/home/developer/me"})
		}, Fail, "dev mounts /home/me of the host at /home/developer/me", true},
		{"another file where init.sh goes", testEnv, func(in *Environment) {
			devMount(in, plan.InitScriptTarget).Source = "/etc/shadow"
		}, Fail, "dev mounts /etc/shadow of the host at /etc/bulkhead/init.sh", false},
		{"the socket under another name", testEnv, func(in *Environment) {
			in.Dev.Mounts = append(in.Dev.Mounts, engine.Mount{Type: "bind", Source: "/run/user/1000/docker.sock", Target: "/run/d.sock"})
		}, Fail, "the Docker socket is mounted into dev", true},
		{"the socket in the holder, with the opt-in", socket, func(in *Environment) {
			in.Holder.Mounts = []engine.Mount{{Type: "bind", Source: plan.DockerSocketPath, Target: plan.DockerSocketPath}}
		}, Fail, "the Docker socket is mounted into holder", false},
		{"writable credentials", testEnv, func(in *Environment) {
			devMount(in, "/etc/bulkhead/github").ReadOnly = false
		}, Fail, "dev can write to /etc/bulkhead/github, which bh mounts read-only", true},
		{"another environment's volume", testEnv, func(in *Environment) {
			in.Dev.Mounts = append(in.Dev.Mounts, engine.Mount{Type: "volume", Source: "bulkhead-other_workspace", Target: "/other"})
		}, Warn, "dev mounts the volume bulkhead-other_workspace at /other", true},
		{"a volume missing", testEnv, func(in *Environment) {
			in.Dev.Mounts = slices.DeleteFunc(in.Dev.Mounts, func(m engine.Mount) bool { return m.Target == "/etc/bulkhead/tokens" })
		}, Warn, "dev lacks its mount at /etc/bulkhead/tokens — bh restart main recreates it", true},
		{"the opt-in not in effect", socket, func(in *Environment) {
			in.Dev.Mounts = slices.DeleteFunc(in.Dev.Mounts, func(m engine.Mount) bool { return m.Target == plan.DockerSocketPath })
		}, Warn, "dev lacks its mount at /var/run/docker.sock", false},

		// Ports.
		{"every exposed port published", testEnv, func(in *Environment) {
			in.Holder.PublishAllPorts = true
		}, Fail, "holder publishes every exposed port on all interfaces", true},
		{"ssh on IPv6 as well", testEnv, func(in *Environment) {
			in.Holder.Ports = append(in.Holder.Ports, engine.PortBinding{ContainerPort: "22/tcp", HostIP: "::", HostPort: "22001"})
		}, Fail, "ssh is published on 127.0.0.1:22001, [::]:22001; the settings say 127.0.0.1:22001", true},
		{"ssh on another port", testEnv, func(in *Environment) {
			in.Holder.Ports[0].HostPort = "2222"
		}, Fail, "ssh is published on 127.0.0.1:2222; the settings say 127.0.0.1:22001", true},
		{"ssh not published", testEnv, func(in *Environment) {
			in.Holder.Ports = nil
		}, Fail, "ssh is not published; the settings say 127.0.0.1:22001", true},
		{"publish in the settings only", func() plan.Env { env := testEnv; env.Publish = true; return env }(), func(in *Environment) {
			in.Holder.Ports[0].HostIP = "127.0.0.1"
		}, Fail, "ssh is published on 127.0.0.1:22001; the settings say 0.0.0.0:22001", false},

		// GPU.
		{"gpu in the settings only", legacy, func(in *Environment) {
			in.Dev.DeviceRequests = nil
		}, Fail, "dev does not have the GPU request of the settings (gpu) — bh restart main applies it", true},
		{"other cdi devices than chosen", cdi, func(in *Environment) {
			in.Dev.DeviceRequests[0].DeviceIDs = []string{"a"}
		}, Fail, "dev does not have the GPU request of the settings (cdi: a, b)", true},
		{"cdi in the settings, all gpus in the container", cdi, func(in *Environment) {
			in.Dev.DeviceRequests = []engine.DeviceRequest{gpuRequest}
		}, Fail, "dev does not have the GPU request of the settings (cdi: a, b)", true},
		{"a gpu the settings do not ask for", testEnv, func(in *Environment) {
			in.Dev.DeviceRequests = []engine.DeviceRequest{gpuRequest}
		}, Warn, "dev has a device request the settings do not ask for (gpu)", true},

		// The holder's gate.
		{"the self-check still running", testEnv, func(in *Environment) {
			in.Holder.Health = "starting"
		}, Warn, "holder's self-check is still running", true},
		{"no health at all", testEnv, func(in *Environment) {
			in.Holder.Health, in.Holder.Healthcheck = "", nil
		}, Fail, "holder reports no health: nothing vouches for the egress filter", false},
		{"the healthcheck removed", testEnv, func(in *Environment) {
			in.Holder.Healthcheck = nil
		}, Fail, "holder's healthcheck is not the one bh sets (none)", true},
		{"another value for a sysctl", testEnv, func(in *Environment) {
			in.Holder.Sysctls = map[string]string{"net.ipv6.conf.all.disable_ipv6": "0", "net.ipv4.ip_unprivileged_port_start": "0"}
		}, Fail, "holder sets the sysctl net.ipv6.conf.all.disable_ipv6 to 0; bh sets it to 1", true},

		// The two containers together.
		{"a holder that keeps exiting", testEnv, func(in *Environment) {
			in.Holder.Status = "restarting"
		}, Fail, "holder keeps exiting", false},
		{"a holder that restarted alone", testEnv, func(in *Environment) {
			in.Holder.StartedAt = in.Dev.StartedAt.Add(time.Minute)
		}, Fail, "dev has been running since before the holder last started", true},
		{"dev in the namespace of a holder that is gone", testEnv, func(in *Environment) {
			in.Dev.NetworkMode = "container:old-holder"
		}, Fail, "dev is not in the holder's network namespace (network mode: container:old-holder)", true},
		{"dev missing", testEnv, func(in *Environment) {
			in.Dev = nil
		}, Fail, "the dev container is missing (bh up main creates it)", true},

		// Older than the plan.
		{"an image of another bh", testEnv, func(in *Environment) {
			in.Holder.Image = "bulkhead-holder:old"
		}, Warn, "holder runs the image bulkhead-holder:old; this bh would use bulkhead-holder:abc", true},
		{"created from other settings", testEnv, func(in *Environment) {
			in.Plan.AuthorizedKeys = append(in.Plan.AuthorizedKeys, "ssh-ed25519 BBBB two")
		}, Warn, "dev was created from other settings than the current ones — the next bh up main recreates it", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := created(tt.env, tt.env)
			tt.change(&in)
			findings := CheckLocal(in)
			if !has(findings, tt.level, tt.want) {
				t.Errorf("no %s finding with %q:\n%s", tt.level, tt.want, listing(findings))
			}
			if !tt.only {
				return
			}
			for _, f := range findings {
				if f.Level != OK && !f.Standing && !strings.Contains(f.Message, tt.want) {
					t.Errorf("also: %s %s", f.Level, f.Message)
				}
			}
		})
	}
}

// A privileged container must not come with an "ok" about its capabilities
// next to the failure: CapDrop still reads ALL and means nothing.
func TestPrivilegedHidesTheCapabilityChecks(t *testing.T) {
	in := created(testEnv, testEnv)
	in.Dev.Privileged = true
	for _, f := range CheckLocal(in) {
		if f.Level == OK && (strings.Contains(f.Message, "dev has no capabilities") || strings.Contains(f.Message, "dev cannot gain privileges")) {
			t.Errorf("ok next to privileged: %s", f.Message)
		}
	}
}

func TestStoppedContainers(t *testing.T) {
	stop := func(in *Environment) {
		in.Holder.Status, in.Dev.Status = "exited", "exited"
	}

	t.Run("that up would start again are checked", func(t *testing.T) {
		in := created(testEnv, testEnv)
		stop(&in)
		in.Dev.CapAdd = []string{"SYS_PTRACE"}
		findings := CheckLocal(in)
		if !has(findings, Skip, "not running") || !has(findings, Fail, "dev has capabilities bh does not give it") {
			t.Errorf("findings:\n%s", listing(findings))
		}
	})

	t.Run("that up would replace are not", func(t *testing.T) {
		in := created(testEnv, testEnv)
		stop(&in)
		in.Plan.SSHPort, in.Plan.Publish = 23000, true
		findings := CheckLocal(in)
		if !has(findings, Skip, "the stopped containers are not checked") || Failures([]Section{{Findings: findings}}) != 0 {
			t.Errorf("findings:\n%s", listing(findings))
		}
		if got := Worst(findings); got != Skip {
			t.Errorf("Worst = %s, want skip (the standing warning aside, nothing was checked)", got)
		}
	})

	// dev refers to the holder by id: replacing the holder replaces dev.
	t.Run("a dev whose holder is replaced goes with it", func(t *testing.T) {
		in := created(testEnv, testEnv)
		stop(&in)
		in.Holder.Labels = map[string]string{plan.LabelSpec: "other"}
		in.Dev.Privileged = true
		if findings := CheckLocal(in); Failures([]Section{{Findings: findings}}) != 0 {
			t.Errorf("findings:\n%s", listing(findings))
		}
	})

	t.Run("a running container is checked whatever its label", func(t *testing.T) {
		in := created(testEnv, testEnv)
		in.Dev.Labels = map[string]string{}
		in.Dev.Privileged = true
		if findings := CheckLocal(in); !has(findings, Fail, "dev is privileged") {
			t.Errorf("findings:\n%s", listing(findings))
		}
	})

	t.Run("only the holder left after a failed start", func(t *testing.T) {
		in := created(testEnv, testEnv)
		in.Holder.Status, in.Dev = "exited", nil
		if findings := CheckLocal(in); Failures([]Section{{Findings: findings}}) != 0 || !has(findings, Skip, "not running") {
			t.Errorf("findings:\n%s", listing(findings))
		}
	})
}

func TestWhatCannotBeLookedAt(t *testing.T) {
	absent := Environment{Name: "main", Plan: testEnv, Target: testTarget}
	if findings := CheckLocal(absent); len(findings) != 1 || !has(findings, Skip, "not created yet (bh up main creates and starts it)") {
		t.Errorf("no containers:\n%s", listing(findings))
	}

	down := absent
	down.EngineDown = true
	if findings := CheckLocal(down); len(findings) != 1 || !has(findings, Skip, "containers not checked") {
		t.Errorf("engine down:\n%s", listing(findings))
	}

	refused := absent
	refused.InspectErr = errors.New("permission denied")
	if findings := CheckLocal(refused); !has(findings, Fail, "the containers could not be inspected: permission denied") {
		t.Errorf("inspect error:\n%s", listing(findings))
	}

	// Without the whole plan the boundary is still checked; whether the
	// containers are current is not claimed either way.
	unknown := created(testEnv, testEnv)
	unknown.PlanUnknown = "a key of the environment does not resolve"
	unknown.KeysErr = errors.New(`environment "main" refers to key SHA256:x, which is not registered (keys.json)`)
	unknown.Plan.AuthorizedKeys = nil
	unknown.Dev.CapAdd = []string{"NET_RAW"}
	findings := CheckLocal(unknown)
	for _, want := range []struct {
		level Level
		part  string
	}{
		{Fail, "which is not registered"},
		{Skip, "not compared with the current settings: a key of the environment does not resolve"},
		{Fail, "dev has capabilities bh does not give it: NET_RAW"},
	} {
		if !has(findings, want.level, want.part) {
			t.Errorf("no %s finding with %q:\n%s", want.level, want.part, listing(findings))
		}
	}
	if has(findings, Warn, "other settings") {
		t.Errorf("claims the settings changed without knowing them:\n%s", listing(findings))
	}

	if findings := CheckBroken("env.json:\nunexpected end"); Failures([]Section{{Findings: findings}}) != 1 {
		t.Errorf("a broken environment does not count as a failure: %v", findings)
	}
	if findings := CheckRemote(); len(findings) != 1 || findings[0].Level != Skip {
		t.Errorf("remote: %v", findings)
	}
}

// liveness and ops.Status.State are two readings of the same containers:
// whatever ops calls degraded, doctor complains about, and it never fails
// an environment that ops calls running, stopped or starting.
func TestLivenessAgreesWithTheState(t *testing.T) {
	statuses := []string{"", "created", "running", "exited", "restarting", "paused", "dead"}
	healths := []string{"", "none", "starting", "healthy", "unhealthy"}
	modes := []string{plan.JoinMode("holder-id"), "bridge", "container:gone"}

	for _, holderStatus := range statuses {
		for _, health := range healths {
			for _, devStatus := range statuses {
				for _, mode := range modes {
					for _, devFirst := range []bool{false, true} {
						var holder, dev *engine.Description
						if holderStatus != "" {
							holder = &engine.Description{Container: engine.Container{
								ID: "holder-id", Status: holderStatus, Health: health, StartedAt: time.Unix(1000, 0),
							}}
						}
						if devStatus != "" {
							started := time.Unix(1001, 0)
							if devFirst {
								started = time.Unix(999, 0)
							}
							dev = &engine.Description{Container: engine.Container{
								ID: "dev-id", Status: devStatus, NetworkMode: mode, StartedAt: started,
							}}
						}
						current := state(holder, dev)
						findings := liveness("main", holder, dev)
						failed := slices.ContainsFunc(findings, func(f Finding) bool { return f.Level == Fail })
						warned := slices.ContainsFunc(findings, func(f Finding) bool { return f.Level == Warn })

						label := fmt.Sprintf("holder %q (%s), dev %q on %s, dev first %v: %s", holderStatus, health, devStatus, mode, devFirst, current)
						switch current {
						case ops.StateDegraded:
							if !failed && !warned {
								t.Errorf("%s, and doctor has no complaint:\n%s", label, listing(findings))
							}
						case ops.StateStarting:
							if failed || !warned {
								t.Errorf("%s:\n%s", label, listing(findings))
							}
						default:
							if failed || warned {
								t.Errorf("%s, and doctor complains:\n%s", label, listing(findings))
							}
						}
						if strings.Contains(listing(findings), "do not add up") {
							t.Errorf("%s: a degraded state without a named cause:\n%s", label, listing(findings))
						}
					}
				}
			}
		}
	}
}

func TestHostPath(t *testing.T) {
	tests := map[string]string{
		"/home/me/.config/bh/init.sh":              "/home/me/.config/bh/init.sh",
		"/host_mnt/Users/me/init.sh":               "/Users/me/init.sh",
		"/run/desktop/mnt/host/c/Users/me/init.sh": "/c/Users/me/init.sh",
		`C:\Users\me\init.sh`:                      "/c/Users/me/init.sh",
		"/C/Users/me":                              "/c/Users/me",
		"/home/me/":                                "/home/me",
		"/host_mnt":                                "/",
		"/host_mnt/":                               "/",
		"/":                                        "/",
		"":                                         "",
		"/host_mntx/a":                             "/host_mntx/a",
		"/Users/me":                                "/Users/me",
		"/run/desktop/mnt/host/c":                  "/c",
		"/var/lib/docker/volumes/bulkhead-x_home/_data/": "/var/lib/docker/volumes/bulkhead-x_home/_data",
	}
	for path, want := range tests {
		if got := hostPath(path); got != want {
			t.Errorf("hostPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestWorstAndFailures(t *testing.T) {
	tests := []struct {
		name     string
		findings []Finding
		worst    Level
		failures int
	}{
		{"nothing", nil, Skip, 0},
		{"only skipped", []Finding{skip("a")}, Skip, 0},
		{"ok", []Finding{skip("a"), ok("b")}, OK, 0},
		{"a standing warning does not colour", []Finding{ok("a"), standing("b")}, OK, 0},
		{"a warning", []Finding{ok("a"), warn("b"), standing("c")}, Warn, 0},
		{"failures", []Finding{warn("a"), fail("b"), ok("c"), fail("d")}, Fail, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Worst(tt.findings); got != tt.worst {
				t.Errorf("Worst = %s, want %s", got, tt.worst)
			}
			sections := []Section{{Title: "a", Findings: tt.findings}, {Title: "b"}}
			if got := Failures(sections); got != tt.failures {
				t.Errorf("Failures = %d, want %d", got, tt.failures)
			}
		})
	}
}
