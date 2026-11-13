// Package plan turns an environment's settings into the containers that
// realize it. Everything here is a pure function: the invariants of the
// boundary (threat model, "境界の前提条件") are decided in this file and
// pinned by its tests, before anything touches Docker.
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
)

// Label keys bh puts on every resource it creates. The namespace is still
// open (spec draft §12); keep it in this one place.
const (
	labelPrefix = "bulkhead."
	LabelOwner  = labelPrefix + "owner" // which config directory owns the resource
	LabelEnv    = labelPrefix + "env"   // environment id
	LabelRole   = labelPrefix + "role"  // holder, dev, network, or a volume key
	LabelSpec   = labelPrefix + "spec"  // hash of the container spec it was created from
)

// VolumeKeys are the volumes of an environment: three split by lifecycle
// (workspace = the code, home = environment state, nix = tool store) plus
// the ssh host identity.
var VolumeKeys = []string{"workspace", "home", "nix", "hostkeys"}

// InjectKeys are the volumes that carry credentials from the host into the
// environment: written only by bh on the host, mounted read-only in dev.
// They are the names and mount points of the TypeScript version, so what an
// environment had there is still in place after bh migrate.
var InjectKeys = []string{"github", "tokens"}

// AllVolumeKeys are all the volumes of an environment: what up creates and
// destroy removes.
func AllVolumeKeys() []string { return append(append([]string{}, VolumeKeys...), InjectKeys...) }

// Workspace is where the user's work lives inside dev: what an editor opens.
const Workspace = "/home/developer/workspace"

var volumeTargets = map[string]string{
	"home":      "/home/developer",
	"workspace": Workspace,
	"nix":       "/nix",
	"hostkeys":  "/etc/bulkhead/hostkeys",
	"github":    "/etc/bulkhead/github",
	"tokens":    "/etc/bulkhead/tokens",
}

// InitScriptTarget is where dev finds the init script (bh-guest init).
const InitScriptTarget = "/etc/bulkhead/init.sh"

// Env is what an environment's settings say about its containers.
type Env struct {
	ID             string   // identifies the Docker resources; never changes
	Name           string   // what people call it; becomes the hostname
	SSHPort        int      // host side; the container side is always 22
	Publish        bool     // true binds the ssh port on all interfaces (opt-in)
	Restart        string   // "no" or "unless-stopped"; applied to both containers
	AuthorizedKeys []string // public keys allowed to log in

	// DockerSocket mounts the engine's socket into dev: an explicit opt-in
	// that gives up the boundary for this environment (whoever can talk to
	// the engine can do anything on the host). SocketGID is the group that
	// owns the socket as the engine sees it; dev's user is added to it.
	DockerSocket bool
	SocketGID    int

	// InitScript is the host path of the init script, mounted read-only.
	// Empty mounts nothing (bh-guest then has nothing to run).
	InitScript string

	// GPU is "off" (or empty), "legacy" (every GPU, through the NVIDIA
	// runtime) or "cdi" (the CDIDevices). A device request adds no
	// capability: dev still has none. DeviceGIDs are the groups that own the
	// device nodes when those are not open to everyone; dev's user joins them.
	GPU        string
	CDIDevices []string
	DeviceGIDs []int
}

// DockerSocketPath is where the engine's socket is, on the engine's side
// and inside dev.
const DockerSocketPath = "/var/run/docker.sock"

// Target is where and from what the containers are created.
type Target struct {
	Owner       string // id of the config directory creating the resources
	HolderImage string
	DevImage    string
}

// Container is a container to create: its name and the two API structures.
type Container struct {
	Name   string
	Config *container.Config
	Host   *container.HostConfig
}

func project(id string) string { return "bulkhead-" + id }

// Network is the environment's bridge network.
func Network(id string) string { return project(id) }

// HolderName and DevName are the container names.
func HolderName(id string) string { return project(id) + "-holder" }
func DevName(id string) string    { return project(id) + "-dev" }

// Volume is the Docker volume for a key. The "_" separator is what the
// TypeScript version (compose) used, so existing volumes are picked up as-is.
func Volume(id, key string) string { return project(id) + "_" + key }

// Labels are the ownership labels for a resource of this environment.
func Labels(owner, id, role string) map[string]string {
	return map[string]string{LabelOwner: owner, LabelEnv: id, LabelRole: role}
}

// Holder is the container that owns the network namespace and its egress
// filter. It is the only container with a capability (NET_ADMIN) and the
// only one with published ports.
func Holder(env Env, target Target) Container {
	listen := netip.MustParseAddr("127.0.0.1")
	if env.Publish {
		listen = netip.MustParseAddr("0.0.0.0")
	}
	ssh := network.MustParsePort("22/tcp")
	init := true

	spec := Container{
		Name: HolderName(env.ID),
		Config: &container.Config{
			Image: target.HolderImage,
			// dev shares this namespace, so the name shows up there too; the
			// engine refuses a hostname on the container that joins.
			Hostname:     env.Name,
			ExposedPorts: network.PortSet{ssh: struct{}{}},
			// Healthy only after firewall.sh proved the filter bites. Up waits
			// for this before it starts dev: the fail-safe gate.
			Healthcheck: &container.HealthConfig{
				Test:        []string{"CMD", "/usr/local/bin/healthcheck.sh"},
				Interval:    5 * time.Second,
				Timeout:     5 * time.Second,
				Retries:     6,
				StartPeriod: 30 * time.Second,
			},
			Labels: Labels(target.Owner, env.ID, "holder"),
		},
		Host: &container.HostConfig{
			NetworkMode: container.NetworkMode(Network(env.ID)),
			PortBindings: network.PortMap{
				ssh: {{HostIP: listen, HostPort: strconv.Itoa(env.SSHPort)}},
			},
			Init:    &init,
			CapDrop: []string{"ALL"},
			CapAdd:  []string{"NET_ADMIN"},
			Sysctls: map[string]string{
				// Applies to the shared namespace, so it covers dev too.
				"net.ipv6.conf.all.disable_ipv6": "1",
				// Lets the non-root sshd in dev bind port 22. This is Docker's
				// default; stated so that we do not depend on a default.
				"net.ipv4.ip_unprivileged_port_start": "0",
			},
			// The ready marker lives here and must not survive a restart.
			Tmpfs:         map[string]string{"/run": ""},
			RestartPolicy: restartPolicy(env),
		},
	}
	return withSpecLabel(spec)
}

// Dev is the container the user works in. It has no capabilities, no network
// namespace of its own and no published ports: it joins the holder's
// namespace and lives behind the holder's filter.
//
// holderID is the id of the running holder container. The spec label does
// not depend on it; whether a dev container still points at the current
// holder is a separate check (its NetworkMode).
func Dev(env Env, target Target, holderID string) Container {
	init := true
	mounts := make([]mount.Mount, 0, len(VolumeKeys)+len(InjectKeys)+2)
	for _, key := range VolumeKeys {
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeVolume,
			Source: Volume(env.ID, key),
			Target: volumeTargets[key],
		})
	}
	for _, key := range InjectKeys {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeVolume,
			Source:   Volume(env.ID, key),
			Target:   volumeTargets[key],
			ReadOnly: true,
		})
	}
	if env.InitScript != "" {
		// The one file of the host that dev sees. A bind given as a mount
		// fails when the file is missing, instead of making a directory.
		mounts = append(mounts, mount.Mount{
			Type: mount.TypeBind, Source: env.InitScript, Target: InitScriptTarget, ReadOnly: true,
		})
	}

	spec := Container{
		Name: DevName(env.ID),
		Config: &container.Config{
			Image:  target.DevImage,
			Env:    []string{"SSH_AUTHORIZED_KEYS=" + strings.Join(env.AuthorizedKeys, "\n")},
			Labels: Labels(target.Owner, env.ID, "dev"),
		},
		Host: &container.HostConfig{
			Init:          &init,
			CapDrop:       []string{"ALL"},
			SecurityOpt:   []string{"no-new-privileges:true"},
			Mounts:        mounts,
			RestartPolicy: restartPolicy(env),
		},
	}
	if env.DockerSocket {
		// A bind given as a mount fails when the source is missing, instead
		// of silently creating an empty directory in its place.
		spec.Host.Mounts = append(spec.Host.Mounts, mount.Mount{
			Type: mount.TypeBind, Source: DockerSocketPath, Target: DockerSocketPath,
		})
	}
	switch env.GPU {
	case "legacy":
		spec.Host.DeviceRequests = []container.DeviceRequest{{Count: -1, Capabilities: [][]string{{"gpu"}}}}
	case "cdi":
		spec.Host.DeviceRequests = []container.DeviceRequest{{Driver: "cdi", DeviceIDs: append([]string{}, env.CDIDevices...)}}
	}
	spec.Host.GroupAdd = groups(env)
	spec = withSpecLabel(spec)
	spec.Host.NetworkMode = container.NetworkMode(JoinMode(holderID))
	return spec
}

// JoinMode is the network mode of a container that joins holderID's namespace.
func JoinMode(holderID string) string { return "container:" + holderID }

// restartPolicy is the same for both containers on purpose: a holder that
// restarts alone gets a fresh namespace and strands dev without network.
func restartPolicy(env Env) container.RestartPolicy {
	mode := container.RestartPolicyDisabled
	if env.Restart == "unless-stopped" {
		mode = container.RestartPolicyUnlessStopped
	}
	return container.RestartPolicy{Name: mode}
}

// withSpecLabel stamps the container with a hash of everything it is created
// from. Up compares it with the label of an existing container to decide
// whether that container still matches the settings.
func withSpecLabel(spec Container) Container {
	encoded, err := json.Marshal(struct {
		Config *container.Config
		Host   *container.HostConfig
	}{spec.Config, spec.Host})
	if err != nil {
		// Both structures are plain data from this file.
		panic("plan: encoding a container spec: " + err.Error())
	}
	sum := sha256.Sum256(encoded)
	spec.Config.Labels[LabelSpec] = hex.EncodeToString(sum[:])[:16]
	return spec
}

// Names the TypeScript version (through Docker Compose) gave the resources
// of the environment called name. The volumes are not here: they had the
// names Volume gives, which is why they can be used as they are.
func LegacyHolderName(name string) string { return project(name) + "-holder-1" }
func LegacyDevName(name string) string    { return project(name) + "-dev-1" }
func LegacyNetwork(name string) string    { return project(name) + "_default" }

// LegacyProjectLabel is the label Compose put on its containers; its value
// for an environment is LegacyProject.
const LegacyProjectLabel = "com.docker.compose.project"

// LegacyProject is the Compose project of the environment called name.
func LegacyProject(name string) string { return project(name) }

// groups are the supplementary groups of dev's user: the owner of the
// Docker socket (with the opt-in) and of the GPU device nodes, without
// repeats. dev's user is not root, and neither is writable by others.
func groups(env Env) []string {
	var gids []int
	if env.DockerSocket {
		gids = append(gids, env.SocketGID)
	}
	if env.GPU == "cdi" {
		gids = append(gids, env.DeviceGIDs...)
	}
	slices.Sort(gids)
	gids = slices.Compact(gids)
	if len(gids) == 0 {
		return nil
	}
	list := make([]string, len(gids))
	for i, gid := range gids {
		list[i] = strconv.Itoa(gid)
	}
	return list
}
