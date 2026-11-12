package engine

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// Description is a container in the detail `bh doctor` checks: what it may
// do (privileges, capabilities, namespaces) and what of the host it reaches
// (mounts, ports, devices). It is plain data, so that the checks do not
// depend on the API types, and the same shape describes a container that
// exists (Describe) and one that is only planned (DescribeSpec).
type Description struct {
	Container

	Name  string
	Image string // the reference it was created from, not the resolved id
	// Healthcheck is the test command; nil when the container has none.
	Healthcheck []string

	Privileged  bool
	CapAdd      []string
	CapDrop     []string
	SecurityOpt []string
	// Namespaces shared with the host or with another container. Empty
	// means the container's own; the engine reports IpcMode as "private"
	// or "shareable" for that.
	PidMode    string
	IpcMode    string
	UsernsMode string

	// Mounts are the mounts in effect, however they were asked for
	// (--mount, -v, --volumes-from), sorted by target.
	Mounts []Mount
	// Ports are the requested host bindings, sorted by container port.
	Ports []PortBinding
	// PublishAllPorts: every exposed port gets a host port on all interfaces.
	PublishAllPorts bool
	Sysctls         map[string]string
	RestartPolicy   string
	// Devices are the host paths of device nodes mapped into the container.
	Devices        []string
	DeviceRequests []DeviceRequest
	GroupAdd       []string
}

// Mount is one mount of a container.
type Mount struct {
	Type string // bind, volume, tmpfs, npipe, ...
	// Source is the volume's name for a volume and the path on the engine's
	// host for a bind.
	Source   string
	Target   string
	ReadOnly bool
}

// PortBinding publishes a container port on the host.
type PortBinding struct {
	ContainerPort string // "22/tcp"
	// HostIP is the address listened on; "" means every interface.
	HostIP   string
	HostPort string
}

// DeviceRequest asks a device driver for devices (GPUs).
type DeviceRequest struct {
	Driver       string
	Count        int
	DeviceIDs    []string
	Capabilities [][]string
}

// Describe looks a container up by name, in detail. A missing container is
// (nil, nil).
func (c *Client) Describe(ctx context.Context, name string) (*Description, error) {
	res, err := c.api.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if errdefs.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	described := describe(res.Container)
	return &described, nil
}

// DescribeSpec is the description a container created from config and host
// has before the engine adds anything: what an existing container is
// compared with, and what the fake engine answers Describe from. Its mounts
// are the ones asked for (Mounts and Binds); the engine lists those in
// effect.
func DescribeSpec(name string, config *container.Config, host *container.HostConfig) Description {
	info := container.InspectResponse{Name: "/" + name, Config: config, HostConfig: host}
	if host != nil {
		info.Mounts = requestedMounts(host)
	}
	return describe(info)
}

// summarize is the part of an inspection every command needs.
func summarize(info container.InspectResponse) Container {
	found := Container{ID: info.ID}
	if info.State != nil {
		found.Status = string(info.State.Status)
		// An unparsable time stays zero, which callers treat as "unknown".
		found.StartedAt, _ = time.Parse(time.RFC3339Nano, info.State.StartedAt)
		if info.State.Health != nil {
			found.Health = string(info.State.Health.Status)
		}
	}
	if info.HostConfig != nil {
		found.NetworkMode = string(info.HostConfig.NetworkMode)
	}
	if info.Config != nil {
		found.Labels = info.Config.Labels
	}
	return found
}

func describe(info container.InspectResponse) Description {
	described := Description{Container: summarize(info), Name: strings.TrimPrefix(info.Name, "/")}
	if config := info.Config; config != nil {
		described.Image = config.Image
		// ["NONE"] is how a healthcheck inherited from the image is turned off.
		if check := config.Healthcheck; check != nil && len(check.Test) > 0 && check.Test[0] != "NONE" {
			described.Healthcheck = slices.Clone(check.Test)
		}
	}

	// The effective list, not HostConfig.Mounts: a bind given with -v or
	// taken over with --volumes-from is only here.
	for _, point := range info.Mounts {
		source := point.Source
		if point.Type == mount.TypeVolume && point.Name != "" {
			source = point.Name
		}
		described.Mounts = append(described.Mounts, Mount{
			Type: string(point.Type), Source: source, Target: point.Destination, ReadOnly: !point.RW,
		})
	}
	slices.SortFunc(described.Mounts, func(a, b Mount) int { return cmp.Compare(a.Target, b.Target) })

	host := info.HostConfig
	if host == nil {
		return described
	}
	described.Privileged = host.Privileged
	described.CapAdd = slices.Clone(host.CapAdd)
	described.CapDrop = slices.Clone(host.CapDrop)
	described.SecurityOpt = slices.Clone(host.SecurityOpt)
	described.PidMode = string(host.PidMode)
	described.IpcMode = string(host.IpcMode)
	described.UsernsMode = string(host.UsernsMode)
	described.PublishAllPorts = host.PublishAllPorts
	described.Sysctls = host.Sysctls
	described.RestartPolicy = string(host.RestartPolicy.Name)
	described.GroupAdd = slices.Clone(host.GroupAdd)

	for port, bindings := range host.PortBindings {
		for _, binding := range bindings {
			ip := ""
			if binding.HostIP.IsValid() {
				ip = binding.HostIP.String()
			}
			described.Ports = append(described.Ports, PortBinding{
				ContainerPort: port.String(), HostIP: ip, HostPort: binding.HostPort,
			})
		}
	}
	slices.SortFunc(described.Ports, func(a, b PortBinding) int {
		return cmp.Or(
			cmp.Compare(a.ContainerPort, b.ContainerPort),
			cmp.Compare(a.HostIP, b.HostIP),
			cmp.Compare(a.HostPort, b.HostPort),
		)
	})

	for _, device := range host.Devices {
		described.Devices = append(described.Devices, device.PathOnHost)
	}
	for _, request := range host.DeviceRequests {
		described.DeviceRequests = append(described.DeviceRequests, DeviceRequest{
			Driver: request.Driver, Count: request.Count,
			DeviceIDs: slices.Clone(request.DeviceIDs), Capabilities: request.Capabilities,
		})
	}
	return described
}

// requestedMounts turns what a container is created with into the list an
// inspection would show. Binds are read in their plain form only
// (source:target[:options], source a path or a volume name).
func requestedMounts(host *container.HostConfig) []container.MountPoint {
	var points []container.MountPoint
	for _, requested := range host.Mounts {
		point := container.MountPoint{
			Type: requested.Type, Source: requested.Source, Destination: requested.Target, RW: !requested.ReadOnly,
		}
		if requested.Type == mount.TypeVolume {
			point.Name = requested.Source
		}
		points = append(points, point)
	}
	for _, bind := range host.Binds {
		parts := strings.Split(bind, ":")
		if len(parts) < 2 {
			continue
		}
		point := container.MountPoint{Type: mount.TypeBind, Source: parts[0], Destination: parts[1], RW: true}
		if !strings.HasPrefix(parts[0], "/") {
			point.Type, point.Name = mount.TypeVolume, parts[0]
		}
		if len(parts) > 2 && slices.Contains(strings.Split(parts[2], ","), "ro") {
			point.RW = false
		}
		points = append(points, point)
	}
	return points
}
