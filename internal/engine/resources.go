package engine

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Container is what bh needs to know about an existing container.
type Container struct {
	ID          string
	Status      string // created, running, exited, ...
	Health      string // "", starting, healthy, unhealthy
	NetworkMode string
	Labels      map[string]string
	// StartedAt is when the process last started (zero if it never did).
	StartedAt time.Time
}

// Running reports whether the container's process is up.
func (c Container) Running() bool { return c.Status == string(container.StateRunning) }

// ImageExists reports whether the engine has the image.
func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	_, err := c.api.ImageInspect(ctx, ref)
	if errdefs.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// EnsureNetwork creates the bridge network unless it exists.
func (c *Client) EnsureNetwork(ctx context.Context, name string, labels map[string]string) error {
	_, err := c.api.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err == nil || !errdefs.IsNotFound(err) {
		return err
	}
	// IPv6 would bypass the IPv4 filter; the holder asserts it is off, and
	// the network does not offer it in the first place.
	ipv6 := false
	_, err = c.api.NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Driver:     "bridge",
		EnableIPv6: &ipv6,
		Labels:     labels,
	})
	return err
}

// EnsureVolume creates the volume unless it exists. An existing volume is
// used as it is: its labels cannot be changed after creation.
func (c *Client) EnsureVolume(ctx context.Context, name string, labels map[string]string) error {
	_, err := c.api.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err == nil || !errdefs.IsNotFound(err) {
		return err
	}
	_, err = c.api.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Labels: labels})
	return err
}

// VolumeExists reports whether the engine has the volume.
func (c *Client) VolumeExists(ctx context.Context, name string) (bool, error) {
	_, err := c.api.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if errdefs.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// Container looks a container up by name. A missing container is (nil, nil).
func (c *Client) Container(ctx context.Context, name string) (*Container, error) {
	res, err := c.api.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if errdefs.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	found := summarize(res.Container)
	return &found, nil
}

// CreateContainer creates (does not start) a container and returns its id.
func (c *Client) CreateContainer(ctx context.Context, name string, config *container.Config, host *container.HostConfig) (string, error) {
	res, err := c.api.ContainerCreate(ctx, client.ContainerCreateOptions{Name: name, Config: config, HostConfig: host})
	if err != nil {
		return "", fmt.Errorf("creating %s: %w", name, err)
	}
	return res.ID, nil
}

func (c *Client) StartContainer(ctx context.Context, id string) error {
	_, err := c.api.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

// StopContainer stops a container and leaves it in place: a stopped
// container keeps its volumes referenced, so cleanup tools do not take them
// for unused.
func (c *Client) StopContainer(ctx context.Context, id string) error {
	_, err := c.api.ContainerStop(ctx, id, client.ContainerStopOptions{})
	return err
}

// RemoveContainer removes a container (running or not). Its named volumes stay.
func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	_, err := c.api.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// RemoveVolume removes a volume; a missing one is not an error.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	_, err := c.api.VolumeRemove(ctx, name, client.VolumeRemoveOptions{})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// RemoveNetwork removes a network; a missing one is not an error.
func (c *Client) RemoveNetwork(ctx context.Context, name string) error {
	_, err := c.api.NetworkRemove(ctx, name, client.NetworkRemoveOptions{})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// LogsTail returns the last lines of a container's output (stdout and stderr
// interleaved as the engine recorded them).
func (c *Client) LogsTail(ctx context.Context, id string, lines int) (string, error) {
	res, err := c.api.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       strconv.Itoa(lines),
	})
	if err != nil {
		return "", err
	}
	defer res.Close()
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, res); err != nil {
		return "", err
	}
	return out.String(), nil
}
