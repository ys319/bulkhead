package engine

import (
	"encoding/json"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
)

// An inspection as the engine sends it, of a container made by hand with
// about everything bh doctor looks for: `docker run --privileged
// --cap-add SYS_ADMIN --security-opt seccomp=unconfined --pid host
// -v /home/me:/host -v data:/data:ro -p 8080:80 --device /dev/fuse
// --gpus all ...`. Written after the API's documented shape; it has not
// been taken from a running engine.
const inspected = `{
  "Id": "4f6c0d1e2a3b",
  "Name": "/bulkhead-3f9a2c1e7b04-dev",
  "State": {
    "Status": "running", "Running": true, "StartedAt": "2026-11-12T08:15:30.123456789Z",
    "Health": {"Status": "healthy", "FailingStreak": 0, "Log": []}
  },
  "Image": "sha256:0123456789abcdef",
  "HostConfig": {
    "Binds": ["/home/me:/host", "data:/data:ro"],
    "NetworkMode": "container:9a8b7c6d5e4f",
    "PortBindings": {
      "80/tcp": [{"HostIp": "", "HostPort": "8080"}],
      "22/tcp": [{"HostIp": "127.0.0.1", "HostPort": "22001"}, {"HostIp": "::1", "HostPort": "22001"}]
    },
    "RestartPolicy": {"Name": "unless-stopped", "MaximumRetryCount": 0},
    "CapAdd": ["SYS_ADMIN"],
    "CapDrop": null,
    "GroupAdd": ["998"],
    "IpcMode": "private",
    "PidMode": "host",
    "Privileged": true,
    "PublishAllPorts": true,
    "SecurityOpt": ["seccomp=unconfined"],
    "UsernsMode": "host",
    "Sysctls": {"net.ipv6.conf.all.disable_ipv6": "1"},
    "Devices": [{"PathOnHost": "/dev/fuse", "PathInContainer": "/dev/fuse", "CgroupPermissions": "rwm"}],
    "DeviceRequests": [
      {"Driver": "", "Count": -1, "DeviceIDs": null, "Capabilities": [["gpu"]], "Options": {}},
      {"Driver": "cdi", "Count": 0, "DeviceIDs": ["nvidia.com/gpu=0"], "Capabilities": null, "Options": null}
    ],
    "Mounts": [{"Type": "volume", "Source": "bulkhead-3f9a2c1e7b04_home", "Target": "/home/developer"}]
  },
  "Mounts": [
    {"Type": "volume", "Name": "bulkhead-3f9a2c1e7b04_home", "Source": "/var/lib/docker/volumes/bulkhead-3f9a2c1e7b04_home/_data",
     "Destination": "/home/developer", "Driver": "local", "Mode": "z", "RW": true, "Propagation": ""},
    {"Type": "bind", "Source": "/home/me", "Destination": "/host", "Mode": "", "RW": true, "Propagation": "rprivate"},
    {"Type": "volume", "Name": "data", "Source": "/var/lib/docker/volumes/data/_data",
     "Destination": "/data", "Driver": "local", "Mode": "ro", "RW": false, "Propagation": ""}
  ],
  "Config": {
    "Image": "bulkhead-dev-base:0123456789ab",
    "Labels": {"bulkhead.env": "3f9a2c1e7b04", "bulkhead.spec": "0011223344556677"},
    "Healthcheck": {"Test": ["CMD", "/usr/local/bin/healthcheck.sh"], "Interval": 5000000000}
  }
}`

func TestDescribeReadsAnInspection(t *testing.T) {
	var info container.InspectResponse
	if err := json.Unmarshal([]byte(inspected), &info); err != nil {
		t.Fatal(err)
	}
	got := describe(info)
	want := Description{
		Container: Container{
			ID: "4f6c0d1e2a3b", Status: "running", Health: "healthy",
			NetworkMode: "container:9a8b7c6d5e4f",
			Labels:      map[string]string{"bulkhead.env": "3f9a2c1e7b04", "bulkhead.spec": "0011223344556677"},
			StartedAt:   time.Date(2026, 11, 12, 8, 15, 30, 123456789, time.UTC),
		},
		Name:        "bulkhead-3f9a2c1e7b04-dev",
		Image:       "bulkhead-dev-base:0123456789ab",
		Healthcheck: []string{"CMD", "/usr/local/bin/healthcheck.sh"},
		Privileged:  true,
		CapAdd:      []string{"SYS_ADMIN"},
		SecurityOpt: []string{"seccomp=unconfined"},
		PidMode:     "host", IpcMode: "private", UsernsMode: "host",
		// The mounts in effect: the bind given with -v is nowhere else.
		Mounts: []Mount{
			{Type: "volume", Source: "data", Target: "/data", ReadOnly: true},
			{Type: "volume", Source: "bulkhead-3f9a2c1e7b04_home", Target: "/home/developer"},
			{Type: "bind", Source: "/home/me", Target: "/host"},
		},
		Ports: []PortBinding{
			{ContainerPort: "22/tcp", HostIP: "127.0.0.1", HostPort: "22001"},
			{ContainerPort: "22/tcp", HostIP: "::1", HostPort: "22001"},
			{ContainerPort: "80/tcp", HostIP: "", HostPort: "8080"},
		},
		PublishAllPorts: true,
		Sysctls:         map[string]string{"net.ipv6.conf.all.disable_ipv6": "1"},
		RestartPolicy:   "unless-stopped",
		Devices:         []string{"/dev/fuse"},
		DeviceRequests: []DeviceRequest{
			{Count: -1, Capabilities: [][]string{{"gpu"}}},
			{Driver: "cdi", DeviceIDs: []string{"nvidia.com/gpu=0"}},
		},
		GroupAdd: []string{"998"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("describe:\n got  %+v\n want %+v", got, want)
	}
}

// The little an inspection can lack must not take describe down: a
// container in an odd state is exactly when doctor is run.
func TestDescribeToleratesMissingParts(t *testing.T) {
	got := describe(container.InspectResponse{ID: "abc", Name: "/x"})
	if got.ID != "abc" || got.Name != "x" || got.Healthcheck != nil || len(got.Mounts) != 0 {
		t.Errorf("describe = %+v", got)
	}

	// A healthcheck switched off is no healthcheck.
	off := describe(container.InspectResponse{Config: &container.Config{
		Healthcheck: &container.HealthConfig{Test: []string{"NONE"}},
	}})
	if off.Healthcheck != nil {
		t.Errorf("Healthcheck = %v, want none", off.Healthcheck)
	}
}

// DescribeSpec is what the planned containers are compared through, and
// what the fake engine answers from: the mounts asked for either way
// (Mounts and Binds) come out as an inspection lists them.
func TestDescribeSpecListsTheRequestedMounts(t *testing.T) {
	got := DescribeSpec("x", &container.Config{Image: "img"}, &container.HostConfig{
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: "vol", Target: "/vol"},
			{Type: mount.TypeBind, Source: "/etc/init.sh", Target: "/init.sh", ReadOnly: true},
		},
		Binds: []string{"/home/me:/host", "named:/named:ro", "/var/run/docker.sock:/var/run/docker.sock:rw,z"},
		PortBindings: network.PortMap{
			network.MustParsePort("22/tcp"): {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "22001"}},
		},
	})
	wantMounts := []Mount{
		{Type: "bind", Source: "/home/me", Target: "/host"},
		{Type: "bind", Source: "/etc/init.sh", Target: "/init.sh", ReadOnly: true},
		{Type: "volume", Source: "named", Target: "/named", ReadOnly: true},
		{Type: "bind", Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"},
		{Type: "volume", Source: "vol", Target: "/vol"},
	}
	if !reflect.DeepEqual(got.Mounts, wantMounts) {
		t.Errorf("mounts:\n got  %+v\n want %+v", got.Mounts, wantMounts)
	}
	wantPorts := []PortBinding{{ContainerPort: "22/tcp", HostIP: "127.0.0.1", HostPort: "22001"}}
	if got.Name != "x" || got.Image != "img" || !reflect.DeepEqual(got.Ports, wantPorts) {
		t.Errorf("describe = %+v", got)
	}
}
