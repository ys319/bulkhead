package plan

import (
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
)

var (
	testEnv = Env{
		ID:             "3f9a2c1e7b04",
		Name:           "main",
		SSHPort:        22001,
		Restart:        "unless-stopped",
		AuthorizedKeys: []string{"ssh-ed25519 AAAA one", "ssh-ed25519 BBBB two"},
	}
	testTarget = Target{Owner: "cfg-1", HolderImage: "bulkhead-holder:abc", DevImage: "bulkhead-dev-base:def"}
	sshPort    = network.MustParsePort("22/tcp")
)

// The five invariants of the boundary (threat model). If one of these tests
// has to change, the threat model changes first.

func TestDevHasNoPrivileges(t *testing.T) {
	dev := Dev(testEnv, testTarget, "holder-id")

	if !slices.Equal(dev.Host.CapDrop, []string{"ALL"}) {
		t.Errorf("CapDrop = %v, want [ALL]", dev.Host.CapDrop)
	}
	if len(dev.Host.CapAdd) != 0 {
		t.Errorf("dev must not add capabilities, got %v", dev.Host.CapAdd)
	}
	if dev.Host.Privileged {
		t.Error("dev must not be privileged")
	}
	if !slices.Contains(dev.Host.SecurityOpt, "no-new-privileges:true") {
		t.Errorf("SecurityOpt = %v, want no-new-privileges", dev.Host.SecurityOpt)
	}
}

func TestDevMountsOnlyItsOwnVolumes(t *testing.T) {
	dev := Dev(testEnv, testTarget, "holder-id")

	if len(dev.Host.Binds) != 0 {
		t.Errorf("dev must not bind host paths, got %v", dev.Host.Binds)
	}
	want := map[string]string{
		"bulkhead-3f9a2c1e7b04_home":      "/home/developer",
		"bulkhead-3f9a2c1e7b04_workspace": "/home/developer/workspace",
		"bulkhead-3f9a2c1e7b04_nix":       "/nix",
		"bulkhead-3f9a2c1e7b04_hostkeys":  "/etc/bulkhead/hostkeys",
		"bulkhead-3f9a2c1e7b04_github":    "/etc/bulkhead/github",
		"bulkhead-3f9a2c1e7b04_tokens":    "/etc/bulkhead/tokens",
	}
	if len(dev.Host.Mounts) != len(want) {
		t.Fatalf("mounts = %+v", dev.Host.Mounts)
	}
	for _, m := range dev.Host.Mounts {
		if m.Type != "volume" {
			t.Errorf("%s is a %s mount: only named volumes (no docker socket, no home directory)", m.Target, m.Type)
		}
		if want[m.Source] != m.Target {
			t.Errorf("unexpected mount %s -> %s", m.Source, m.Target)
		}
		// What the host hands in is read-only inside: the environment cannot
		// change what bh wrote (the direction is host to guest only).
		injected := strings.HasPrefix(m.Target, "/etc/bulkhead/") && m.Target != "/etc/bulkhead/hostkeys"
		if m.ReadOnly != injected {
			t.Errorf("%s: read-only = %v", m.Target, m.ReadOnly)
		}
	}
}

func TestPublishedPortsLiveOnTheHolderOnly(t *testing.T) {
	dev := Dev(testEnv, testTarget, "holder-id")
	if len(dev.Host.PortBindings) != 0 || len(dev.Config.ExposedPorts) != 0 {
		t.Errorf("dev must not publish ports: %v / %v", dev.Host.PortBindings, dev.Config.ExposedPorts)
	}
	if got := string(dev.Host.NetworkMode); got != "container:holder-id" {
		t.Errorf("dev NetworkMode = %q, want the holder's namespace", got)
	}
	if dev.Config.Hostname != "" {
		t.Error("the engine refuses a hostname on a container that joins a namespace")
	}

	holder := Holder(testEnv, testTarget)
	bindings := holder.Host.PortBindings[sshPort]
	if len(bindings) != 1 || bindings[0].HostIP.String() != "127.0.0.1" || bindings[0].HostPort != "22001" {
		t.Errorf("holder bindings = %+v, want loopback:22001", bindings)
	}
	if string(holder.Host.NetworkMode) != "bulkhead-3f9a2c1e7b04" {
		t.Errorf("holder NetworkMode = %q", holder.Host.NetworkMode)
	}
}

func TestPublishIsOptIn(t *testing.T) {
	env := testEnv
	env.Publish = true
	bindings := Holder(env, testTarget).Host.PortBindings[sshPort]
	if len(bindings) != 1 || bindings[0].HostIP.String() != "0.0.0.0" {
		t.Errorf("published bindings = %+v, want 0.0.0.0", bindings)
	}
}

func TestHolderHoldsTheOnlyCapabilityAndTheGate(t *testing.T) {
	holder := Holder(testEnv, testTarget)

	if !slices.Equal(holder.Host.CapDrop, []string{"ALL"}) || !slices.Equal(holder.Host.CapAdd, []string{"NET_ADMIN"}) {
		t.Errorf("holder caps: drop %v add %v, want ALL / NET_ADMIN", holder.Host.CapDrop, holder.Host.CapAdd)
	}
	if holder.Host.Privileged {
		t.Error("holder must not be privileged")
	}
	if len(holder.Host.Mounts) != 0 || len(holder.Host.Binds) != 0 {
		t.Error("holder mounts nothing")
	}
	check := holder.Config.Healthcheck
	if check == nil || !slices.Equal(check.Test, []string{"CMD", "/usr/local/bin/healthcheck.sh"}) {
		t.Fatalf("healthcheck = %+v: without it nothing gates dev", check)
	}
	if _, ok := holder.Host.Tmpfs["/run"]; !ok {
		t.Error("/run must be a tmpfs: a ready marker that survives a restart opens the gate early")
	}
	if holder.Host.Sysctls["net.ipv6.conf.all.disable_ipv6"] != "1" {
		t.Error("IPv6 must be off: it would bypass the IPv4 filter")
	}
	if holder.Config.Hostname != "main" {
		t.Errorf("hostname = %q, want the environment name", holder.Config.Hostname)
	}
}

func TestRestartPolicyIsTheSameOnBoth(t *testing.T) {
	for _, mode := range []string{"no", "unless-stopped"} {
		env := testEnv
		env.Restart = mode
		holder, dev := Holder(env, testTarget), Dev(env, testTarget, "h")
		if holder.Host.RestartPolicy != dev.Host.RestartPolicy {
			t.Errorf("%s: holder %v, dev %v", mode, holder.Host.RestartPolicy, dev.Host.RestartPolicy)
		}
		if string(holder.Host.RestartPolicy.Name) != mode {
			t.Errorf("%s: policy = %q", mode, holder.Host.RestartPolicy.Name)
		}
	}
	// Anything unknown falls to the safe side.
	env := testEnv
	env.Restart = "always"
	if got := Holder(env, testTarget).Host.RestartPolicy.Name; got != container.RestartPolicyDisabled {
		t.Errorf("unknown mode gave %q, want no", got)
	}
}

func TestAuthorizedKeysReachDev(t *testing.T) {
	dev := Dev(testEnv, testTarget, "h")
	want := "SSH_AUTHORIZED_KEYS=ssh-ed25519 AAAA one\nssh-ed25519 BBBB two"
	if !slices.Contains(dev.Config.Env, want) {
		t.Errorf("env = %q", dev.Config.Env)
	}
}

func TestLabelsSayWhoOwnsWhat(t *testing.T) {
	holder, dev := Holder(testEnv, testTarget), Dev(testEnv, testTarget, "h")
	for role, c := range map[string]Container{"holder": holder, "dev": dev} {
		labels := c.Config.Labels
		if labels[LabelOwner] != "cfg-1" || labels[LabelEnv] != "3f9a2c1e7b04" || labels[LabelRole] != role {
			t.Errorf("%s labels = %v", role, labels)
		}
		if len(labels[LabelSpec]) != 16 {
			t.Errorf("%s spec label = %q", role, labels[LabelSpec])
		}
	}
	if !strings.HasPrefix(holder.Name, "bulkhead-3f9a2c1e7b04-") || !strings.HasPrefix(dev.Name, "bulkhead-3f9a2c1e7b04-") {
		t.Errorf("names = %q, %q", holder.Name, dev.Name)
	}
}

func TestSpecLabelFollowsTheSettings(t *testing.T) {
	spec := func(c Container) string { return c.Config.Labels[LabelSpec] }

	if spec(Holder(testEnv, testTarget)) != spec(Holder(testEnv, testTarget)) {
		t.Error("the same settings must give the same spec")
	}

	port := testEnv
	port.SSHPort = 22002
	if spec(Holder(port, testTarget)) == spec(Holder(testEnv, testTarget)) {
		t.Error("a port change must change the holder spec")
	}
	if spec(Dev(port, testTarget, "h")) != spec(Dev(testEnv, testTarget, "h")) {
		t.Error("the ssh port is not part of the dev container")
	}

	keys := testEnv
	keys.AuthorizedKeys = []string{"ssh-ed25519 CCCC three"}
	if spec(Dev(keys, testTarget, "h")) == spec(Dev(testEnv, testTarget, "h")) {
		t.Error("a key change must change the dev spec")
	}

	image := testTarget
	image.DevImage = "bulkhead-dev-base:new"
	if spec(Dev(testEnv, image, "h")) == spec(Dev(testEnv, testTarget, "h")) {
		t.Error("a new image must change the dev spec")
	}

	// A recreated holder has a new id; that alone is not a settings change.
	if spec(Dev(testEnv, testTarget, "holder-1")) != spec(Dev(testEnv, testTarget, "holder-2")) {
		t.Error("the dev spec must not depend on the holder's container id")
	}

	shm := testEnv
	shm.ShmSize = 16 << 30
	if spec(Dev(shm, testTarget, "h")) == spec(Dev(testEnv, testTarget, "h")) {
		t.Error("another size of /dev/shm must change the dev spec")
	}
	if spec(Holder(shm, testTarget)) != spec(Holder(testEnv, testTarget)) {
		t.Error("the size of /dev/shm is not part of the holder")
	}
}

// /dev/shm is as large as the machine's settings say, in dev only, and it
// stays dev's own: a larger one is never had by sharing the host's.
func TestSharedMemoryIsDevsOwnAndAsLargeAsAsked(t *testing.T) {
	env := testEnv
	env.ShmSize = 8 << 30
	dev := Dev(env, testTarget, "holder-id")
	if dev.Host.ShmSize != 8<<30 {
		t.Errorf("dev ShmSize = %d", dev.Host.ShmSize)
	}
	if dev.Host.IpcMode != "" {
		t.Errorf("dev IpcMode = %q", dev.Host.IpcMode)
	}
	if _, mounted := dev.Host.Tmpfs["/dev/shm"]; mounted {
		t.Error("/dev/shm is a tmpfs mount of its own")
	}
	if holder := Holder(env, testTarget); holder.Host.ShmSize != 0 {
		t.Errorf("holder ShmSize = %d", holder.Host.ShmSize)
	}
	// Without a size, the engine decides.
	if dev := Dev(testEnv, testTarget, "holder-id"); dev.Host.ShmSize != 0 {
		t.Errorf("dev ShmSize without a setting = %d", dev.Host.ShmSize)
	}
}

// The socket is in dev only when the environment asked for it — and then
// it is mounted as a bind that cannot silently become an empty directory,
// with the socket's group and nothing else added.
func TestDockerSocketIsOptIn(t *testing.T) {
	base := Env{ID: "e1", Name: "main", SSHPort: 22001, Restart: "no"}
	target := Target{Owner: "cfg", HolderImage: "holder:1", DevImage: "dev:1"}

	plain := Dev(base, target, "h")
	for _, m := range plain.Host.Mounts {
		if m.Type != mount.TypeVolume {
			t.Errorf("a dev without the opt-in has a %s mount of %s", m.Type, m.Source)
		}
	}
	if len(plain.Host.GroupAdd) != 0 {
		t.Errorf("GroupAdd = %v", plain.Host.GroupAdd)
	}

	opted := base
	opted.DockerSocket, opted.SocketGID = true, 998
	dev := Dev(opted, target, "h")
	var binds []mount.Mount
	for _, m := range dev.Host.Mounts {
		if m.Type != mount.TypeVolume {
			binds = append(binds, m)
		}
	}
	if len(binds) != 1 || binds[0].Type != mount.TypeBind || binds[0].Source != DockerSocketPath || binds[0].Target != DockerSocketPath {
		t.Errorf("binds = %+v", binds)
	}
	if len(dev.Host.GroupAdd) != 1 || dev.Host.GroupAdd[0] != "998" {
		t.Errorf("GroupAdd = %v", dev.Host.GroupAdd)
	}
	// Everything else about dev stays as tight as before.
	if dev.Host.Privileged || len(dev.Host.CapAdd) != 0 || dev.Host.CapDrop[0] != "ALL" {
		t.Errorf("the opt-in loosened more than the socket: %+v", dev.Host)
	}
	// The holder is not affected at all.
	if Holder(opted, target).Config.Labels[LabelSpec] != Holder(base, target).Config.Labels[LabelSpec] {
		t.Error("the opt-in changed the holder")
	}
	// Turning it on (or a different group) recreates dev.
	if dev.Config.Labels[LabelSpec] == plain.Config.Labels[LabelSpec] {
		t.Error("the opt-in did not change dev's spec")
	}
}

func TestInitScriptIsTheOnlyHostFileAndReadOnly(t *testing.T) {
	env := testEnv
	env.InitScript = "/home/me/.config/bh/init.sh"
	dev := Dev(env, testTarget, "h")
	var binds []mount.Mount
	for _, m := range dev.Host.Mounts {
		if m.Type != mount.TypeVolume {
			binds = append(binds, m)
		}
	}
	if len(binds) != 1 || binds[0].Type != mount.TypeBind || binds[0].Source != env.InitScript ||
		binds[0].Target != InitScriptTarget || !binds[0].ReadOnly {
		t.Errorf("binds = %+v", binds)
	}
	if len(dev.Host.Binds) != 0 {
		t.Errorf("Binds = %v (a bind given this way makes a directory when the file is missing)", dev.Host.Binds)
	}
}

func TestGPUAddsDevicesAndNothingElse(t *testing.T) {
	base := Env{ID: "e1", Name: "main", SSHPort: 22001, Restart: "no", GPU: "off"}
	target := Target{Owner: "cfg", HolderImage: "holder:1", DevImage: "dev:1"}
	plain := Dev(base, target, "h")
	if len(plain.Host.DeviceRequests) != 0 {
		t.Errorf("gpu off: %+v", plain.Host.DeviceRequests)
	}

	legacy := base
	legacy.GPU = "legacy"
	got := Dev(legacy, target, "h").Host
	if len(got.DeviceRequests) != 1 || got.DeviceRequests[0].Count != -1 || got.DeviceRequests[0].Driver != "" ||
		!slices.Equal(got.DeviceRequests[0].Capabilities[0], []string{"gpu"}) {
		t.Errorf("legacy: %+v", got.DeviceRequests)
	}

	cdi := base
	cdi.GPU, cdi.CDIDevices, cdi.DeviceGIDs = "cdi", []string{"nvidia.com/gpu=0"}, []int{44, 44, 110}
	cdi.DockerSocket, cdi.SocketGID = true, 110
	got = Dev(cdi, target, "h").Host
	if len(got.DeviceRequests) != 1 || got.DeviceRequests[0].Driver != "cdi" ||
		!slices.Equal(got.DeviceRequests[0].DeviceIDs, []string{"nvidia.com/gpu=0"}) {
		t.Errorf("cdi: %+v", got.DeviceRequests)
	}
	if !slices.Equal(got.GroupAdd, []string{"44", "110"}) {
		t.Errorf("GroupAdd = %v, want the device groups and the socket's, once each", got.GroupAdd)
	}
	// A device is not a capability: dev still has none, and is not privileged.
	if got.Privileged || len(got.CapAdd) != 0 || got.CapDrop[0] != "ALL" {
		t.Errorf("the GPU loosened more than the devices: %+v", got)
	}
	// The holder never gets a device.
	if len(Holder(cdi, target).Host.DeviceRequests) != 0 {
		t.Error("the holder got a device")
	}
	// Device groups of a setting that is not cdi are ignored.
	off := base
	off.DeviceGIDs = []int{44}
	if len(Dev(off, target, "h").Host.GroupAdd) != 0 {
		t.Error("device groups were added without gpu: cdi")
	}
}
