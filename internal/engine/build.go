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

func (CLIBuilder) Build(ctx context.Context, ref string, src image.Source, progress io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", "build", "--tag", ref, "-")
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
