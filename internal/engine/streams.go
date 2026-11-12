package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// LogOptions select what Logs streams.
type LogOptions struct {
	Follow     bool // keep streaming until the context ends
	Tail       int  // the last lines only; 0 for everything
	Timestamps bool
}

// Logs copies a container's output, stdout and stderr interleaved as the
// engine recorded them, to out. With Follow it returns when the container
// is removed or the context ends.
func (c *Client) Logs(ctx context.Context, id string, opts LogOptions, out io.Writer) error {
	tail := "all"
	if opts.Tail > 0 {
		tail = strconv.Itoa(opts.Tail)
	}
	stream, err := c.api.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: true, ShowStderr: true,
		Follow: opts.Follow, Tail: tail, Timestamps: opts.Timestamps,
	})
	if err != nil {
		return err
	}
	defer stream.Close()
	_, err = stdcopy.StdCopy(out, out, stream)
	if ctx.Err() != nil {
		return nil // the caller ended it
	}
	return err
}

// ExecSpec is a command to run inside a running container.
type ExecSpec struct {
	User       string
	Privileged bool
	WorkingDir string
	Env        []string
	Cmd        []string
	// TTY gives the command a terminal. The caller puts its own terminal in
	// raw mode and passes the size.
	TTY bool
	// Size reports the terminal's size; asked at the start and again while
	// the command runs, so that resizing the window reaches it.
	Size func() (height, width uint)
}

// Streams are what a command is connected to. A nil In means no input.
type Streams struct {
	In       io.Reader
	Out, Err io.Writer
}

// Exec runs a command in a running container, connected to the streams,
// and returns its exit status.
func (c *Client) Exec(ctx context.Context, id string, spec ExecSpec, streams Streams) (int, error) {
	options := client.ExecCreateOptions{
		User: spec.User, Privileged: spec.Privileged, WorkingDir: spec.WorkingDir,
		Env: spec.Env, Cmd: spec.Cmd, TTY: spec.TTY,
		AttachStdin: streams.In != nil, AttachStdout: true, AttachStderr: true,
	}
	var size client.ConsoleSize
	if spec.TTY && spec.Size != nil {
		size.Height, size.Width = spec.Size()
		options.ConsoleSize = size
	}
	created, err := c.api.ExecCreate(ctx, id, options)
	if err != nil {
		return 0, err
	}
	attached, err := c.api.ExecAttach(ctx, created.ID, client.ExecAttachOptions{TTY: spec.TTY, ConsoleSize: size})
	if err != nil {
		return 0, err
	}
	defer attached.Close()

	if streams.In != nil {
		// Input is copied until it ends or the command does. When the
		// command ends first this goroutine stays blocked on the read; the
		// callers are commands that exit right after.
		go func() {
			_, _ = io.Copy(attached.Conn, streams.In)
			_ = attached.CloseWrite()
		}()
	}
	if spec.TTY && spec.Size != nil {
		done := make(chan struct{})
		defer close(done)
		go c.followSize(ctx, created.ID, spec.Size, size, done)
	}

	// With a terminal the two outputs arrive as one raw stream.
	if spec.TTY {
		_, err = io.Copy(streams.Out, attached.Reader)
	} else {
		_, err = stdcopy.StdCopy(streams.Out, streams.Err, attached.Reader)
	}
	if err != nil && ctx.Err() == nil {
		return 0, err
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	result, err := c.api.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if err != nil {
		return 0, err
	}
	return result.ExitCode, nil
}

// followSize passes changes of the terminal's size on to the command.
// Polling works the same on every platform (Windows has no SIGWINCH).
func (c *Client) followSize(ctx context.Context, execID string, measure func() (uint, uint), last client.ConsoleSize, done <-chan struct{}) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		height, width := measure()
		if height == 0 || width == 0 || (height == last.Height && width == last.Width) {
			continue
		}
		last = client.ConsoleSize{Height: height, Width: width}
		_, _ = c.api.ExecResize(ctx, execID, client.ExecResizeOptions{Height: height, Width: width})
	}
}

// runWithInput is runOnce for a command that reads its standard input:
// the input goes over the attached connection, never through arguments or
// the environment, where `docker inspect` and the process list would show it.
func (c *Client) runWithInput(ctx context.Context, config *container.Config, host *container.HostConfig, input []byte) (string, error) {
	config.OpenStdin, config.StdinOnce, config.AttachStdin = true, true, true
	created, err := c.api.ContainerCreate(ctx, client.ContainerCreateOptions{Config: config, HostConfig: host})
	if err != nil {
		return "", err
	}
	defer c.api.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})

	// Attached before the start, so that nothing the command reads is lost.
	attached, err := c.api.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{Stream: true, Stdin: true})
	if err != nil {
		return "", err
	}
	defer attached.Close()
	wait := c.api.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})
	if _, err := c.api.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return "", err
	}
	if _, err := attached.Conn.Write(input); err != nil {
		return "", fmt.Errorf("sending the input: %w", err)
	}
	if err := attached.CloseWrite(); err != nil {
		return "", fmt.Errorf("ending the input: %w", err)
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
