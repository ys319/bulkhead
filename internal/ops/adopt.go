package ops

import (
	"context"
	"errors"
	"fmt"

	"github.com/ys319/bulkhead/internal/engine"
	"github.com/ys319/bulkhead/internal/plan"
)

// Adoption is what Adopt did.
type Adoption struct {
	// Started: the environment was running and is running again, in the new
	// containers. False: it was stopped and stays stopped.
	Started bool
	// Leftovers are old resources that could not be cleaned up. They are in
	// nobody's way; they are reported so they can be removed by hand.
	Leftovers []string
}

// Adopt takes over an environment the TypeScript version ran through Docker
// Compose: it replaces the two Compose containers and their network with
// bh's own. env.ID is the environment's name there.
//
// The volumes are the point of the exercise and the one thing Adopt never
// touches: no volume is created over, copied or removed. They are also
// never left without a container that refers to them — the new containers
// are created before the old ones go — so that no cleanup running at the
// wrong moment can take them for unused.
//
// The old containers stay until the new ones are up behind a verified
// filter. If that fails, the new ones are removed, the old ones are started
// again and the error says so. Adopt can be run again after any failure.
func (r Runner) Adopt(ctx context.Context, env plan.Env, target plan.Target) (Adoption, error) {
	eng := r.Engine
	oldHolder, err := r.legacyContainer(ctx, plan.LegacyHolderName(env.ID), env.ID)
	if err != nil {
		return Adoption{}, err
	}
	oldDev, err := r.legacyContainer(ctx, plan.LegacyDevName(env.ID), env.ID)
	if err != nil {
		return Adoption{}, err
	}
	current, err := r.Inspect(ctx, env.ID)
	if err != nil {
		return Adoption{}, err
	}
	running := func(c *engine.Container) bool { return c != nil && c.Running() }
	// The new containers running means an earlier run got that far.
	wasRunning := running(oldHolder) || running(oldDev) || running(current.Holder) || running(current.Dev)

	// From here on the volumes are referred to by the new containers too.
	if err := r.prepare(ctx, env, target); err != nil {
		return Adoption{}, err
	}

	if wasRunning {
		// The old holder has the ssh port; it has to let go of it first.
		for _, old := range []*engine.Container{oldDev, oldHolder} {
			if running(old) {
				if err := eng.StopContainer(ctx, old.ID); err != nil {
					return Adoption{}, fmt.Errorf("stopping the old containers: %w", err)
				}
			}
		}
		if err := r.Up(ctx, env, target); err != nil {
			return Adoption{}, r.backOut(ctx, env.ID, oldHolder, oldDev, running(oldHolder) || running(oldDev), err)
		}
	}

	adoption := Adoption{Started: wasRunning}
	for _, old := range []*engine.Container{oldDev, oldHolder} {
		if old == nil {
			continue
		}
		if err := eng.RemoveContainer(ctx, old.ID); err != nil {
			adoption.Leftovers = append(adoption.Leftovers, fmt.Sprintf("an old container (%s): %v", old.ID[:min(12, len(old.ID))], err))
		}
	}
	if err := eng.RemoveNetwork(ctx, plan.LegacyNetwork(env.ID)); err != nil {
		adoption.Leftovers = append(adoption.Leftovers, fmt.Sprintf("the old network %s: %v", plan.LegacyNetwork(env.ID), err))
	}
	return adoption, nil
}

// legacyContainer looks up a container of the TypeScript version and
// refuses one that merely has the name: Adopt stops and removes what this
// returns.
func (r Runner) legacyContainer(ctx context.Context, name, envName string) (*engine.Container, error) {
	found, err := r.Engine.Container(ctx, name)
	if err != nil || found == nil {
		return nil, err
	}
	if got, want := found.Labels[plan.LegacyProjectLabel], plan.LegacyProject(envName); got != want {
		return nil, fmt.Errorf("the container %s is not one Docker Compose made for this environment (its %s label is %q, not %q) — not touching it",
			name, plan.LegacyProjectLabel, got, want)
	}
	return found, nil
}

// prepare makes the network, the volumes (existing ones are used as they
// are) and both containers, without starting anything.
func (r Runner) prepare(ctx context.Context, env plan.Env, target plan.Target) error {
	eng := r.Engine
	if err := eng.EnsureNetwork(ctx, plan.Network(env.ID), plan.Labels(target.Owner, env.ID, "network")); err != nil {
		return fmt.Errorf("preparing the network: %w", err)
	}
	for _, key := range plan.VolumeKeys {
		if err := eng.EnsureVolume(ctx, plan.Volume(env.ID, key), plan.Labels(target.Owner, env.ID, key)); err != nil {
			return fmt.Errorf("preparing the %s volume: %w", key, err)
		}
	}
	current, err := r.Inspect(ctx, env.ID)
	if err != nil {
		return err
	}
	holderID := ""
	if current.Holder != nil {
		holderID = current.Holder.ID
	} else {
		spec := plan.Holder(env, target)
		if holderID, err = eng.CreateContainer(ctx, spec.Name, spec.Config, spec.Host); err != nil {
			return err
		}
	}
	if current.Dev == nil {
		spec := plan.Dev(env, target, holderID)
		if _, err := eng.CreateContainer(ctx, spec.Name, spec.Config, spec.Host); err != nil {
			return err
		}
	}
	return nil
}

// backOut returns to the old containers after the new ones failed to come
// up: the new ones go (the old ones still refer to the volumes), the old
// ones start again, holder first and dev only behind its verified filter.
func (r Runner) backOut(ctx context.Context, id string, oldHolder, oldDev *engine.Container, restart bool, cause error) error {
	// Finish even if the user gave up waiting: half a way back is the worst place.
	ctx = context.WithoutCancel(ctx)
	undo := func() error {
		if err := r.Release(ctx, id); err != nil {
			return err
		}
		if !restart || oldHolder == nil {
			return nil
		}
		if err := r.Engine.StartContainer(ctx, oldHolder.ID); err != nil {
			return err
		}
		if _, err := r.waitForGate(ctx, plan.LegacyHolderName(id)); err != nil {
			return err
		}
		if oldDev == nil {
			return nil
		}
		return r.Engine.StartContainer(ctx, oldDev.ID)
	}
	if err := undo(); err != nil {
		return errors.Join(cause, fmt.Errorf("and going back to the old containers failed too: %w\n(nothing was removed from the volumes; start the environment with the TypeScript version's bh)", err))
	}
	if restart {
		return fmt.Errorf("%w\n(the old containers were started again; nothing was migrated)", cause)
	}
	return fmt.Errorf("%w\n(nothing was migrated)", cause)
}

// Release removes the environment's containers and network and keeps its
// volumes: what remains is exactly the data.
func (r Runner) Release(ctx context.Context, id string) error {
	if err := r.Down(ctx, id); err != nil {
		return err
	}
	if err := r.removeContainers(ctx, id); err != nil {
		return err
	}
	if err := r.Engine.RemoveNetwork(ctx, plan.Network(id)); err != nil {
		return fmt.Errorf("removing the network: %w", err)
	}
	return nil
}
