package engine

import (
	"context"
	"fmt"
	"io"
	"os/exec"

	"github.com/ys319/bulkhead/internal/image"
)

// CLIBuilder builds images by shelling out to `docker build`, feeding the
// context on stdin. It is the one place bh still needs the docker CLI:
// driving BuildKit through the Engine API needs a session implementation
// (spec draft §12, build path). The CLI resolves the engine the same way
// ResolveEndpoint does, so both talk to the same daemon.
type CLIBuilder struct{}

func (CLIBuilder) Build(ctx context.Context, ref string, src image.Source, opts image.Options, progress io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", BuildArgs(ref, src, opts)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	cmd.Stdout = progress
	cmd.Stderr = progress
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting docker build: %w", err)
	}
	writeErr := src.WriteContext(stdin)
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("docker build %s: %w", ref, err)
	}
	return writeErr
}

// BuildArgs are the arguments of the docker CLI for a build whose context
// comes on stdin.
func BuildArgs(ref string, src image.Source, opts image.Options) []string {
	args := []string{"build", "--tag", ref}
	if src.Dockerfile != "" {
		args = append(args, "--file", src.Dockerfile)
	}
	if opts.Pull {
		args = append(args, "--pull")
	}
	if opts.NoCache {
		args = append(args, "--no-cache")
	}
	return append(args, "-")
}
