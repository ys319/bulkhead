package engine

import (
	"slices"
	"testing"

	"github.com/moby/moby/api/types/mount"
)

// The container that writes credentials into a volume: it has the volume
// and a shell, and nothing to send anything anywhere with.
func TestVolumeJobSpec(t *testing.T) {
	config, host := volumeJobSpec(VolumeJob{
		Image: "bulkhead-holder:abc", Volume: "bulkhead-web_github", Target: "/g", Script: "cat > /g/token",
	})
	if config.Image != "bulkhead-holder:abc" || config.User != "0:0" {
		t.Errorf("image %q, user %q", config.Image, config.User)
	}
	// The holder image's own entrypoint (the firewall) is replaced.
	if !slices.Equal(config.Entrypoint, []string{"sh", "-c"}) || !slices.Equal(config.Cmd, []string{"cat > /g/token"}) {
		t.Errorf("entrypoint %v, cmd %v", config.Entrypoint, config.Cmd)
	}
	if len(config.Env) != 0 {
		t.Errorf("env = %v", config.Env)
	}
	if host.NetworkMode != "none" {
		t.Errorf("network mode = %q", host.NetworkMode)
	}
	if !slices.Equal(host.CapDrop, []string{"ALL"}) || !slices.Equal(host.CapAdd, []string{"CHOWN", "DAC_OVERRIDE", "FOWNER"}) {
		t.Errorf("capabilities: drop %v, add %v", host.CapDrop, host.CapAdd)
	}
	if !slices.Equal(host.SecurityOpt, []string{"no-new-privileges:true"}) || host.Privileged {
		t.Errorf("security options = %v, privileged = %v", host.SecurityOpt, host.Privileged)
	}
	want := mount.Mount{Type: mount.TypeVolume, Source: "bulkhead-web_github", Target: "/g"}
	if len(host.Mounts) != 1 || host.Mounts[0] != want || len(host.Binds) != 0 {
		t.Errorf("mounts = %+v, binds = %v", host.Mounts, host.Binds)
	}
	// Nothing of the host: no ports, no devices.
	if len(host.PortBindings) != 0 || len(host.Devices) != 0 || len(host.DeviceRequests) != 0 {
		t.Errorf("host = %+v", host)
	}
}
