// Package ops carries out the lifecycle of an environment against the engine.
package ops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/ys319/bulkhead/internal/engine"
	"github.com/ys319/bulkhead/internal/plan"
)

// Engine is the part of the Docker Engine the lifecycle needs. *engine.Client
// implements it; tests use a fake.
type Engine interface {
	EnsureNetwork(ctx context.Context, name string, labels map[string]string) error
	EnsureVolume(ctx context.Context, name string, labels map[string]string) error
	Container(ctx context.Context, name string) (*engine.Container, error)
	CreateContainer(ctx context.Context, name string, config *container.Config, host *container.HostConfig) (string, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string) error
	RemoveContainer(ctx context.Context, id string) error
	RemoveVolume(ctx context.Context, name string) error
	RemoveNetwork(ctx context.Context, name string) error
	LogsTail(ctx context.Context, id string, lines int) (string, error)
}

// Runner runs lifecycle operations.
type Runner struct {
	Engine Engine
	// Sleep and GateTimeout are for tests; zero values mean time.Sleep and
	// DefaultGateTimeout.
	Sleep       func(time.Duration)
	GateTimeout time.Duration
}

// DefaultGateTimeout bounds the wait for the holder's self-assert: its
// healthcheck allows a 30s start period plus 6 retries 5s apart.
const DefaultGateTimeout = 90 * time.Second

const gatePoll = 500 * time.Millisecond

// GateError means the holder did not prove its filter, so dev was not
// started. Logs holds the tail of the holder's output.
type GateError struct {
	Reason string
	Logs   string
}

func (e *GateError) Error() string {
	msg := "the holder did not become healthy (" + e.Reason + ") — dev was not started"
	if logs := strings.TrimSpace(e.Logs); logs != "" {
		msg += "\n" + logs
	}
	return msg
}

// Up brings the environment to "both containers running, dev behind a
// verified filter". Existing containers are kept when they still match the
// settings and recreated when they do not.
//
// The order is the point: dev is created and started only after the holder
// reports healthy, and dev is never left attached to a namespace other than
// the current holder's.
func (r Runner) Up(ctx context.Context, env plan.Env, target plan.Target) error {
	eng := r.Engine

	if err := eng.EnsureNetwork(ctx, plan.Network(env.ID), plan.Labels(target.Owner, env.ID, "network")); err != nil {
		return fmt.Errorf("preparing the network: %w", err)
	}
	for _, key := range plan.AllVolumeKeys() {
		if err := eng.EnsureVolume(ctx, plan.Volume(env.ID, key), plan.Labels(target.Owner, env.ID, key)); err != nil {
			return fmt.Errorf("preparing the %s volume: %w", key, err)
		}
	}

	holderSpec := plan.Holder(env, target)
	holder, err := eng.Container(ctx, holderSpec.Name)
	if err != nil {
		return err
	}
	dev, err := eng.Container(ctx, plan.DevName(env.ID))
	if err != nil {
		return err
	}

	// A holder that no longer matches the settings goes, and dev goes with
	// it: dev refers to the holder by container id.
	if holder != nil && !sameSpec(holder, holderSpec) {
		if dev != nil {
			if err := eng.RemoveContainer(ctx, dev.ID); err != nil {
				return err
			}
			dev = nil
		}
		if err := eng.RemoveContainer(ctx, holder.ID); err != nil {
			return err
		}
		holder = nil
	}

	holderID := ""
	if holder != nil {
		holderID = holder.ID
	}
	if holder == nil {
		holderID, err = eng.CreateContainer(ctx, holderSpec.Name, holderSpec.Config, holderSpec.Host)
		if err != nil {
			return err
		}
	}
	if holder == nil || !holder.Running() {
		// Starting the holder makes a fresh namespace. A dev that is still
		// running sits in the old one; stop it so it rejoins on start.
		if dev != nil && dev.Running() {
			if err := eng.StopContainer(ctx, dev.ID); err != nil {
				return err
			}
			dev.Status = "exited"
		}
		if err := eng.StartContainer(ctx, holderID); err != nil {
			return fmt.Errorf("starting the holder: %w", err)
		}
	}

	gate, err := r.waitForGate(ctx, holderSpec.Name)
	if err != nil {
		// A holder that failed its self-assert must not be left to its
		// restart policy: it would keep restarting with nobody waiting for
		// it. Stopped, it keeps its logs, and the next up starts over.
		var closed *GateError
		if errors.As(err, &closed) {
			_ = eng.StopContainer(context.WithoutCancel(ctx), holderID)
		}
		return err
	}

	devSpec := plan.Dev(env, target, holderID)
	if dev != nil && (!sameSpec(dev, devSpec) || dev.NetworkMode != plan.JoinMode(holderID)) {
		if err := eng.RemoveContainer(ctx, dev.ID); err != nil {
			return err
		}
		dev = nil
	}
	devID := ""
	if dev != nil {
		devID = dev.ID
	}
	if dev == nil {
		devID, err = eng.CreateContainer(ctx, devSpec.Name, devSpec.Config, devSpec.Host)
		if err != nil {
			return err
		}
	}
	// A dev that has been running since before the holder last started sits
	// in the namespace of the holder's previous run: no network, and not
	// behind the current filter. That happens when the holder restarts on
	// its own (a crash, a manual `docker restart`).
	if dev != nil && dev.Running() && dev.StartedAt.Before(gate.StartedAt) {
		if err := eng.StopContainer(ctx, devID); err != nil {
			return err
		}
		dev.Status = "exited"
	}
	if dev == nil || !dev.Running() {
		if err := eng.StartContainer(ctx, devID); err != nil {
			return fmt.Errorf("starting dev: %w", err)
		}
	}
	return nil
}

func sameSpec(existing *engine.Container, want plan.Container) bool {
	return existing.Labels[plan.LabelSpec] == want.Config.Labels[plan.LabelSpec]
}

// waitForGate blocks until the holder's healthcheck passes and returns the
// holder as it is then. The healthcheck passes only after firewall.sh
// installed the filter and proved it bites.
func (r Runner) waitForGate(ctx context.Context, holderName string) (*engine.Container, error) {
	sleep, timeout := r.Sleep, r.GateTimeout
	if sleep == nil {
		sleep = time.Sleep
	}
	if timeout == 0 {
		timeout = DefaultGateTimeout
	}

	fail := func(id, reason string) (*engine.Container, error) {
		logs, _ := r.Engine.LogsTail(ctx, id, 20)
		return nil, &GateError{Reason: reason, Logs: logs}
	}

	for waited := time.Duration(0); ; waited += gatePoll {
		holder, err := r.Engine.Container(ctx, holderName)
		if err != nil {
			return nil, err
		}
		if holder == nil {
			return nil, &GateError{Reason: "the holder container disappeared"}
		}
		switch {
		case !holder.Running():
			// firewall.sh exits when it cannot install or verify the filter.
			return fail(holder.ID, stoppedBecause(holder.Status))
		case holder.Health == string(container.Healthy):
			return holder, nil
		case holder.Health == string(container.Unhealthy):
			return fail(holder.ID, "unhealthy")
		case holder.Health == "" || holder.Health == string(container.NoHealthcheck):
			// Without a healthcheck nothing vouches for the filter.
			return fail(holder.ID, "it has no healthcheck")
		}
		if waited >= timeout {
			return fail(holder.ID, "timed out after "+timeout.String())
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sleep(gatePoll)
	}
}

// stoppedBecause words the status of a holder that is not running.
func stoppedBecause(status string) string {
	switch status {
	case string(container.StateExited):
		return "it exited"
	case string(container.StateRestarting):
		// It exited and its restart policy is bringing it back, again and again.
		return "it keeps exiting"
	}
	return "it is " + status
}

// Down stops the environment, dev first. The containers stay: a stopped
// container keeps its volumes referenced, so `docker volume prune` and
// friends do not take them for unused.
func (r Runner) Down(ctx context.Context, id string) error {
	for _, name := range []string{plan.DevName(id), plan.HolderName(id)} {
		found, err := r.Engine.Container(ctx, name)
		if err != nil {
			return err
		}
		if found == nil || !found.Running() {
			continue
		}
		if err := r.Engine.StopContainer(ctx, found.ID); err != nil {
			return fmt.Errorf("stopping %s: %w", name, err)
		}
	}
	return nil
}

// Destroy removes everything the environment has in Docker: containers,
// volumes (the data, and the credentials bh put there) and the network.
// The caller confirms first.
func (r Runner) Destroy(ctx context.Context, id string) error {
	if err := r.removeContainers(ctx, id); err != nil {
		return err
	}
	if err := r.removeVolumes(ctx, id, plan.AllVolumeKeys()); err != nil {
		return err
	}
	if err := r.Engine.RemoveNetwork(ctx, plan.Network(id)); err != nil {
		return fmt.Errorf("removing the network: %w", err)
	}
	return nil
}

// ResetKeys are the volumes a reset may remove: the state of the
// environment itself. The workspace (the user's work), the ssh host
// identity and the credentials bh handed in are not among them.
var ResetKeys = []string{"home", "nix"}

// Reset takes part of the environment back to what the image provides: it
// removes the containers and the volumes named by keys — the tool store
// (nix) alone, or the home directory with it — and nothing else. The next
// Up creates them fresh. The caller confirms first. A key that is not in
// ResetKeys is refused: no arrangement of arguments makes this remove the
// workspace.
func (r Runner) Reset(ctx context.Context, id string, keys []string) error {
	for _, key := range keys {
		if !slices.Contains(ResetKeys, key) {
			return fmt.Errorf("a reset does not remove the %s volume", key)
		}
	}
	if err := r.Down(ctx, id); err != nil {
		return err
	}
	if err := r.removeContainers(ctx, id); err != nil {
		return err
	}
	return r.removeVolumes(ctx, id, keys)
}

// Archive puts the environment away: it removes the containers, the
// network and every volume but the ssh host identity (plan.ArchiveKeys).
// The caller has put the data into a snapshot, has read that snapshot back
// whole, and has recorded it in the settings — in that order, and before
// this.
func (r Runner) Archive(ctx context.Context, id string) error {
	if err := r.Down(ctx, id); err != nil {
		return err
	}
	if err := r.removeContainers(ctx, id); err != nil {
		return err
	}
	if err := r.removeVolumes(ctx, id, plan.ArchiveKeys()); err != nil {
		return err
	}
	if err := r.Engine.RemoveNetwork(ctx, plan.Network(id)); err != nil {
		return fmt.Errorf("removing the network: %w", err)
	}
	return nil
}

// removeVolumes is the one place volumes are removed. Its callers are the
// operations whose purpose is to delete data — Destroy, Reset and Archive
// — and nothing else may call it: not migrate, not restart, not a repair.
// (A rollback empties volumes without removing them: engine.ReplaceVolume.)
func (r Runner) removeVolumes(ctx context.Context, id string, keys []string) error {
	for _, key := range keys {
		if err := r.Engine.RemoveVolume(ctx, plan.Volume(id, key)); err != nil {
			return fmt.Errorf("removing the %s volume: %w", key, err)
		}
	}
	return nil
}

// Restart stops the environment, removes both containers and brings it up
// again: always two fresh containers. The volumes are untouched.
func (r Runner) Restart(ctx context.Context, env plan.Env, target plan.Target) error {
	if err := r.Down(ctx, env.ID); err != nil {
		return err
	}
	if err := r.removeContainers(ctx, env.ID); err != nil {
		return err
	}
	return r.Up(ctx, env, target)
}

func (r Runner) removeContainers(ctx context.Context, id string) error {
	for _, name := range []string{plan.DevName(id), plan.HolderName(id)} {
		found, err := r.Engine.Container(ctx, name)
		if err != nil {
			return err
		}
		if found == nil {
			continue
		}
		if err := r.Engine.RemoveContainer(ctx, found.ID); err != nil {
			return fmt.Errorf("removing %s: %w", name, err)
		}
	}
	return nil
}

// State is what the two containers of an environment add up to.
type State string

const (
	StateAbsent   State = "not created" // no containers yet
	StateStopped  State = "stopped"
	StateStarting State = "starting" // the holder is still proving its filter
	StateRunning  State = "running"
	// StateDegraded: something runs, but not the whole: one container is
	// down, the holder is unhealthy, or dev is in a stale namespace.
	// `bh up` repairs it.
	StateDegraded State = "degraded"
)

// Status is an environment's containers as they are now; nil = absent.
type Status struct {
	Holder, Dev *engine.Container
}

// Inspect looks up the environment's containers.
func (r Runner) Inspect(ctx context.Context, id string) (Status, error) {
	holder, err := r.Engine.Container(ctx, plan.HolderName(id))
	if err != nil {
		return Status{}, err
	}
	dev, err := r.Engine.Container(ctx, plan.DevName(id))
	if err != nil {
		return Status{}, err
	}
	return Status{Holder: holder, Dev: dev}, nil
}

// State sums the two containers up.
func (s Status) State() State {
	holderUp := s.Holder != nil && s.Holder.Running()
	devUp := s.Dev != nil && s.Dev.Running()
	restarting := func(c *engine.Container) bool {
		return c != nil && c.Status == string(container.StateRestarting)
	}
	switch {
	case s.Holder == nil && s.Dev == nil:
		return StateAbsent
	case restarting(s.Holder), restarting(s.Dev):
		// Exiting and being brought back by the restart policy, in a loop.
		return StateDegraded
	case !holderUp && !devUp:
		return StateStopped
	case holderUp && s.Holder.Health == string(container.Starting) && !devUp:
		return StateStarting
	case holderUp && devUp && s.Holder.Health == string(container.Healthy) &&
		s.Dev.NetworkMode == plan.JoinMode(s.Holder.ID) && !s.Dev.StartedAt.Before(s.Holder.StartedAt):
		return StateRunning
	}
	return StateDegraded
}
