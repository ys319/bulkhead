package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// VolumeTar is a volume read or replaced as a tar stream. The engine has
// no way to hand out a volume's contents with paths left out, and with
// Docker Desktop the volume is not on this machine's file system, so tar
// runs in a short-lived container that has the volume and nothing else.
type VolumeTar struct {
	// Image needs GNU tar, find and a POSIX sh: bh passes the image dev
	// runs from.
	Image  string
	Volume string // must exist
	// Target is where dev mounts the volume. ReplaceVolume mounts it there
	// too: a volume that is still empty then starts with what the image
	// has at that place, as it does under dev.
	Target string
	// LeftOut are paths relative to the volume's root that ReadVolume does
	// not read and ReplaceVolume does not touch.
	LeftOut []string
}

// A left-out path goes into the script that empties the volume.
var leftOutPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

func (job VolumeTar) check() error {
	if job.Image == "" || job.Volume == "" {
		return errors.New("a volume and an image are needed")
	}
	for _, left := range job.LeftOut {
		parts := strings.Split(left, "/")
		if !leftOutPattern.MatchString(left) || containsAny(parts, ".", "..") {
			return fmt.Errorf("%q is not a path inside the volume", left)
		}
	}
	return nil
}

func containsAny(parts []string, names ...string) bool {
	for _, part := range parts {
		for _, name := range names {
			if part == name {
				return true
			}
		}
	}
	return false
}

// readMount is where ReadVolume puts the volume: a place no image has
// anything at, so that mounting changes nothing.
const readMount = "/bh-volume"

// readSpec is the container ReadVolume runs: tar as root with the one
// capability that reads every file whatever its mode, the volume read-only,
// no network.
func readSpec(job VolumeTar) (*container.Config, *container.HostConfig) {
	// Literal names from the root of the volume: no pattern characters,
	// and "cache" further down is not ".cache" at the top.
	args := []string{"--create", "--file=-", "--directory=" + readMount, "--numeric-owner", "--no-wildcards", "--anchored"}
	for _, left := range job.LeftOut {
		args = append(args, "--exclude=./"+left)
	}
	return &container.Config{
			Image: job.Image, User: "0:0",
			Entrypoint: []string{"tar"}, Cmd: append(args, "."),
		}, &container.HostConfig{
			NetworkMode: "none",
			CapDrop:     []string{"ALL"},
			CapAdd:      []string{"DAC_READ_SEARCH"},
			SecurityOpt: []string{"no-new-privileges:true"},
			Mounts:      []mount.Mount{{Type: mount.TypeVolume, Source: job.Volume, Target: readMount, ReadOnly: true}},
		}
}

// replaceScript empties the volume — all but the left-out paths and the
// directories they are in — and unpacks standard input into it. Emptying
// is checked before anything is unpacked: old files under new ones would
// be neither the state before nor the snapshot.
func replaceScript(job VolumeTar) string {
	lines := []string{
		"set -eu",
		"cd '" + job.Target + "'",
		// Everything below deletes relative to where it is. A cd that did
		// not go where it was told (an empty path is "stay here" to some
		// shells) must end it before the first find.
		`[ "$PWD" = '` + job.Target + `' ] || { echo "not in ` + job.Target + `: nothing was removed" >&2; exit 1; }`,
	}
	if len(job.LeftOut) == 0 {
		lines = append(lines,
			"find . -mindepth 1 -delete",
			"left=$(find . -mindepth 1 -print | head -n 1)",
		)
	} else {
		var kept, outside []string
		for _, left := range job.LeftOut {
			kept = append(kept, "-path './"+left+"'")
			outside = append(outside, "! -path './"+left+"' ! -path './"+left+"/*'")
		}
		pruned := `\( ` + strings.Join(kept, " -o ") + ` \) -prune`
		lines = append(lines,
			"find . -mindepth 1 "+pruned+" -o ! -type d -exec rm -f -- {} +",
			// Directories go bottom-up; one that holds a kept path is not
			// empty and stays, which is what rmdir's failure means here.
			"find . -mindepth 1 -depth -type d "+strings.Join(outside, " ")+` -exec rmdir -- {} \; 2>/dev/null || true`,
			"left=$(find . -mindepth 1 "+pruned+" -o ! -type d -print | head -n 1)",
		)
	}
	return strings.Join(append(lines,
		`[ -z "$left" ] || { echo "could not empty the volume: $left is still there" >&2; exit 1; }`,
		"exec tar --extract --file=- --numeric-owner --same-owner --same-permissions",
	), "\n")
}

// replaceSpec is the container ReplaceVolume runs: as root only what it
// takes to remove and create files of any owner and to give them their
// owners and modes back.
func replaceSpec(job VolumeTar) (*container.Config, *container.HostConfig) {
	return &container.Config{
			Image: job.Image, User: "0:0",
			Entrypoint: []string{"sh", "-c"}, Cmd: []string{replaceScript(job)},
		}, &container.HostConfig{
			NetworkMode: "none",
			CapDrop:     []string{"ALL"},
			CapAdd:      []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID"},
			SecurityOpt: []string{"no-new-privileges:true"},
			Mounts:      []mount.Mount{{Type: mount.TypeVolume, Source: job.Volume, Target: job.Target}},
		}
}

// ReadVolume writes the volume's contents to out as a tar stream, names
// relative to the volume's root. The environment should be stopped: files
// that change while they are read make a snapshot of no moment in time.
func (c *Client) ReadVolume(ctx context.Context, job VolumeTar, out io.Writer) error {
	if err := job.check(); err != nil {
		return err
	}
	config, host := readSpec(job)
	return c.runStreaming(ctx, config, host, nil, out)
}

// ReplaceVolume empties the volume, except job.LeftOut, and unpacks the
// tar stream from in. It deletes data: the caller has confirmed, and has
// checked that what comes from in is whole.
func (c *Client) ReplaceVolume(ctx context.Context, job VolumeTar, in io.Reader) error {
	if err := job.check(); err != nil {
		return err
	}
	if !strings.HasPrefix(job.Target, "/") || strings.Trim(job.Target, "/") == "" || strings.ContainsAny(job.Target, `'"\ `) {
		return fmt.Errorf("%q is not a place to mount a volume", job.Target)
	}
	config, host := replaceSpec(job)
	return c.runStreaming(ctx, config, host, in, io.Discard)
}

// restoreScript puts paths of the volume back to what the image has. The
// volume is at readMount, the image's own files are where they always are
// (job.Target), and the paths come as arguments. Each one is removed from
// the volume and, when the image has it, copied over — with the
// directories on the way, which get the image's owners when they have to
// be made (cp --parents).
func restoreScript(job VolumeTar) string {
	return strings.Join([]string{
		"set -eu",
		"[ -d '" + readMount + "' ] && [ -d '" + job.Target + "' ] || { echo \"the volume or the image's directory is not there: nothing was removed\" >&2; exit 1; }",
		"cd '" + job.Target + "'",
		`for path in "$@"; do`,
		`  [ -n "$path" ] || exit 1`,
		`  rm -rf -- "` + readMount + `/$path"`,
		`  if [ -e "$path" ] || [ -L "$path" ]; then`,
		`    cp -a --parents -- "$path" '` + readMount + `/'`,
		`  fi`,
		`done`,
	}, "\n")
}

// restoreSpec is the container RestoreFromImage runs: the volume beside
// the image's files instead of over them, the paths as arguments.
func restoreSpec(job VolumeTar, paths []string) (*container.Config, *container.HostConfig) {
	return &container.Config{
			Image: job.Image, User: "0:0",
			Entrypoint: []string{"sh", "-c", restoreScript(job), "sh"}, Cmd: paths,
		}, &container.HostConfig{
			NetworkMode: "none",
			CapDrop:     []string{"ALL"},
			CapAdd:      []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID"},
			SecurityOpt: []string{"no-new-privileges:true"},
			Mounts:      []mount.Mount{{Type: mount.TypeVolume, Source: job.Volume, Target: readMount}},
		}
}

// RestoreFromImage takes the paths of the volume (relative to its root)
// back to what the image has at job.Target, where dev mounts the volume:
// what the image has there is copied in, what it does not have is gone.
// The rest of the volume is not touched. It deletes data under the paths.
func (c *Client) RestoreFromImage(ctx context.Context, job VolumeTar, paths []string) error {
	job.LeftOut = paths // checked as paths inside the volume
	if err := job.check(); err != nil {
		return err
	}
	if len(paths) == 0 {
		return nil
	}
	if !strings.HasPrefix(job.Target, "/") || strings.Trim(job.Target, "/") == "" || strings.ContainsAny(job.Target, `'"\ `) {
		return fmt.Errorf("%q is not a place to mount a volume", job.Target)
	}
	config, host := restoreSpec(job, paths)
	return c.runStreaming(ctx, config, host, nil, io.Discard)
}

// runStreaming runs a short-lived container whose standard input comes
// from in (nil: none) and whose standard output goes to out, as they come.
// What it writes to standard error is the error's text when it fails.
func (c *Client) runStreaming(ctx context.Context, config *container.Config, host *container.HostConfig, in io.Reader, out io.Writer) error {
	config.AttachStdout, config.AttachStderr = true, true
	if in != nil {
		config.OpenStdin, config.StdinOnce, config.AttachStdin = true, true, true
	}
	created, err := c.api.ContainerCreate(ctx, client.ContainerCreateOptions{Config: config, HostConfig: host})
	if err != nil {
		return err
	}
	defer c.api.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})

	// Attached before the start, so that nothing is lost at either end.
	attached, err := c.api.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{Stream: true, Stdin: in != nil, Stdout: true, Stderr: true})
	if err != nil {
		return err
	}
	defer attached.Close()
	wait := c.api.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})
	if _, err := c.api.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return err
	}

	// A cancelled context has to end the copies below, which block on the
	// connection.
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			attached.Close()
		case <-finished:
		}
	}()

	sent := make(chan error, 1)
	if in == nil {
		sent <- nil
	} else {
		go func() {
			_, err := io.Copy(attached.Conn, in)
			if closeErr := attached.CloseWrite(); err == nil {
				err = closeErr
			}
			sent <- err
		}()
	}
	var complaints limited
	_, copyErr := stdcopy.StdCopy(out, &complaints, attached.Reader)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	var status int64
	select {
	case result := <-wait.Result:
		status = result.StatusCode
	case err := <-wait.Error:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
	// The container's own account comes first: when it stops early, the
	// copies fail as a consequence.
	if status != 0 {
		return fmt.Errorf("exit status %d: %s", status, strings.TrimSpace(complaints.String()))
	}
	if copyErr != nil {
		return fmt.Errorf("receiving the output: %w", copyErr)
	}
	if err := <-sent; err != nil {
		return fmt.Errorf("sending the input: %w", err)
	}
	return nil
}

// limited keeps the start of what is written to it: enough of an error
// output to explain, however much there is.
type limited struct{ bytes.Buffer }

func (l *limited) Write(p []byte) (int, error) {
	if room := 8192 - l.Len(); room > 0 {
		l.Buffer.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
