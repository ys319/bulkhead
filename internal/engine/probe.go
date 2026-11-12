package engine

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// SocketGID finds the group that owns the engine's socket, as seen from the
// engine's side. It has to be asked there: on Docker Desktop the socket
// lives inside a VM and differs from anything visible on this machine.
//
// It runs `stat` in a short-lived container of image (any image with a
// POSIX stat; bh passes its own holder image, which it has anyway) with the
// socket mounted read-only, no network and no capabilities.
func (c *Client) SocketGID(ctx context.Context, image string) (int, error) {
	const path = "/var/run/docker.sock"
	out, err := c.runOnce(ctx,
		&container.Config{
			Image:      image,
			Entrypoint: []string{"stat"},
			Cmd:        []string{"-c", "%F %g", path},
		},
		&container.HostConfig{
			NetworkMode: "none",
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges:true"},
			Mounts:      []mount.Mount{{Type: mount.TypeBind, Source: path, Target: path, ReadOnly: true}},
		})
	if err != nil {
		return 0, fmt.Errorf("cannot find the group of the Docker socket (needed for dockerSocket: true): %w", err)
	}
	return ParseSocketProbe(out)
}

var socketProbe = regexp.MustCompile(`^(.+) (\d+)$`)

// ParseSocketProbe reads the output of `stat -c '%F %g'` on the socket. The
// file type matters as much as the number: what is mounted must really be a
// socket, and an empty answer must not turn into group 0.
func ParseSocketProbe(output string) (int, error) {
	matched := socketProbe.FindStringSubmatch(strings.TrimSpace(output))
	if matched == nil {
		return 0, fmt.Errorf("the Docker socket probe returned something unreadable: %q", strings.TrimSpace(output))
	}
	if matched[1] != "socket" {
		return 0, fmt.Errorf("/var/run/docker.sock on the engine's side is not a socket (it is a %s) — the engine does not offer its socket at the default path (rootless Docker, or a remote engine?); dockerSocket: true cannot work here", matched[1])
	}
	return strconv.Atoi(matched[2])
}

// runOnce runs a container to completion and returns its output. The
// container is removed whatever happens.
func (c *Client) runOnce(ctx context.Context, config *container.Config, host *container.HostConfig) (string, error) {
	created, err := c.api.ContainerCreate(ctx, client.ContainerCreateOptions{Config: config, HostConfig: host})
	if err != nil {
		return "", err
	}
	defer c.api.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})

	// Ask for the exit before starting, so a quick exit cannot be missed.
	wait := c.api.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})
	if _, err := c.api.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return "", err
	}
	var status int64
	select {
	case result := <-wait.Result:
		status = result.StatusCode
	case err := <-wait.Error:
		return "", err
	case <-ctx.Done():
		return "", ctx.Err()
	}

	logs, err := c.api.ContainerLogs(ctx, created.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return "", err
	}
	defer logs.Close()
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, logs); err != nil {
		return "", err
	}
	if status != 0 {
		return "", fmt.Errorf("exit status %d: %s", status, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}
