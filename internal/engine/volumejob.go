package engine

import (
	"context"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
)

// VolumeJob is a shell script that changes the contents of one volume. It
// is how bh writes into a volume: the engine has no API for that, and a
// volume cannot be reached from this machine's file system (with Docker
// Desktop it lives in a VM).
type VolumeJob struct {
	// Image needs a POSIX sh and coreutils. bh passes its own holder image,
	// which is on the machine anyway, so that nothing is pulled.
	Image string
	// Volume must exist: the engine would create a missing one on the
	// spot, without the labels that say whose it is.
	Volume string
	Target string // where the script finds the volume
	Script string
}

// volumeJobSpec is the short-lived container a job runs in. It gets the
// volume and nothing else: no network, and as root only what it takes to
// write files into a directory that belongs to another user and hand them
// over (the volume's files belong to dev's user).
func volumeJobSpec(job VolumeJob) (*container.Config, *container.HostConfig) {
	return &container.Config{
			Image: job.Image,
			// Stated, so that an image with a USER does not change it.
			User:       "0:0",
			Entrypoint: []string{"sh", "-c"},
			Cmd:        []string{job.Script},
		}, &container.HostConfig{
			NetworkMode: "none",
			CapDrop:     []string{"ALL"},
			CapAdd:      []string{"CHOWN", "DAC_OVERRIDE", "FOWNER"},
			SecurityOpt: []string{"no-new-privileges:true"},
			Mounts:      []mount.Mount{{Type: mount.TypeVolume, Source: job.Volume, Target: job.Target}},
		}
}

// WriteVolume runs the job with input as the script's standard input. That
// is where a secret goes: the script, like every argument, shows up in
// `docker inspect` and in the process list.
func (c *Client) WriteVolume(ctx context.Context, job VolumeJob, input []byte) error {
	config, host := volumeJobSpec(job)
	_, err := c.runWithInput(ctx, config, host, input)
	return err
}

// RunInVolume runs a job that reads no input.
func (c *Client) RunInVolume(ctx context.Context, job VolumeJob) error {
	config, host := volumeJobSpec(job)
	_, err := c.runOnce(ctx, config, host)
	return err
}
