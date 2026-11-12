// Package enginetest provides an in-memory stand-in for the Docker Engine,
// for tests of the code that drives it.
package enginetest

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/ys319/bulkhead/internal/engine"
)

// Fake is an in-memory engine that records what was done, in order.
type Fake struct {
	Containers map[string]*engine.Container // by name
	Volumes    map[string]bool              // by name
	Calls      []string

	// HealthAfterStart scripts the holder's health on successive inspections
	// after it is started; the last entry repeats.
	HealthAfterStart []string
	// ExitOnStart makes the holder exit right after start (a failed self-assert).
	ExitOnStart bool
	// ExitOnStartName limits ExitOnStart to the holder of that name.
	ExitOnStartName string
	// Clock is the time of the last start; every start advances it a second.
	Clock time.Time

	nextID      int
	holderPolls int
}

// New returns an engine with no containers whose holder gets healthy at once.
func New() *Fake {
	return &Fake{Containers: map[string]*engine.Container{}, Volumes: map[string]bool{}, HealthAfterStart: []string{"healthy"}}
}

func (f *Fake) log(format string, args ...any) {
	f.Calls = append(f.Calls, fmt.Sprintf(format, args...))
}

func (f *Fake) nameOf(id string) string {
	for name, c := range f.Containers {
		if c.ID == id {
			return name
		}
	}
	return "?" + id
}

// isHolder: bh's holder, or the one Docker Compose named for the
// TypeScript version.
func isHolder(name string) bool {
	return strings.HasSuffix(name, "-holder") || strings.HasSuffix(name, "-holder-1")
}

// AddLegacy puts the containers and volumes of an environment of the
// TypeScript version in place, as Docker Compose left them.
func (f *Fake) AddLegacy(name string, running bool) {
	status := "exited"
	if running {
		status = "running"
	}
	project := "bulkhead-" + name
	labels := map[string]string{"com.docker.compose.project": project}
	f.nextID++
	holderID := fmt.Sprintf("old-%d", f.nextID)
	f.Containers[project+"-holder-1"] = &engine.Container{ID: holderID, Status: status, Health: "healthy", Labels: labels}
	f.nextID++
	f.Containers[project+"-dev-1"] = &engine.Container{
		ID: fmt.Sprintf("old-%d", f.nextID), Status: status, NetworkMode: "container:" + holderID, Labels: labels,
	}
	for _, key := range []string{"workspace", "home", "nix", "hostkeys", "github", "tokens"} {
		f.Volumes[project+"_"+key] = true
	}
}

// ImageExists says yes: tests never build.
func (f *Fake) ImageExists(context.Context, string) (bool, error) { return true, nil }

func (f *Fake) EnsureNetwork(_ context.Context, name string, _ map[string]string) error {
	f.log("network %s", name)
	return nil
}

func (f *Fake) EnsureVolume(_ context.Context, name string, _ map[string]string) error {
	f.log("volume %s", name)
	f.Volumes[name] = true
	return nil
}

func (f *Fake) VolumeExists(_ context.Context, name string) (bool, error) {
	return f.Volumes[name], nil
}

// SocketGID answers the probe for the Docker socket's group.
func (f *Fake) SocketGID(context.Context, string) (int, error) {
	f.log("probe-socket")
	return 998, nil
}

func (f *Fake) Container(_ context.Context, name string) (*engine.Container, error) {
	found, ok := f.Containers[name]
	if !ok {
		return nil, nil
	}
	if isHolder(name) && found.Status == "running" && found.Health != "healthy" {
		step := min(f.holderPolls, len(f.HealthAfterStart)-1)
		found.Health = f.HealthAfterStart[step]
		f.holderPolls++
	}
	copied := *found
	return &copied, nil
}

func (f *Fake) CreateContainer(_ context.Context, name string, config *container.Config, host *container.HostConfig) (string, error) {
	if _, exists := f.Containers[name]; exists {
		return "", fmt.Errorf("conflict: %s exists", name)
	}
	f.nextID++
	id := fmt.Sprintf("id-%d", f.nextID)
	f.Containers[name] = &engine.Container{
		ID:          id,
		Status:      "created",
		NetworkMode: string(host.NetworkMode),
		Labels:      config.Labels,
	}
	f.log("create %s", name)
	return id, nil
}

func (f *Fake) StartContainer(_ context.Context, id string) error {
	name := f.nameOf(id)
	f.log("start %s", name)
	found := f.Containers[name]
	found.Status = "running"
	f.Clock = f.Clock.Add(time.Second)
	found.StartedAt = f.Clock
	if isHolder(name) {
		found.Health = "starting"
		f.holderPolls = 0
		if f.ExitOnStart && (f.ExitOnStartName == "" || f.ExitOnStartName == name) {
			found.Status = "exited"
		}
	}
	return nil
}

func (f *Fake) StopContainer(_ context.Context, id string) error {
	name := f.nameOf(id)
	f.log("stop %s", name)
	f.Containers[name].Status = "exited"
	return nil
}

func (f *Fake) RemoveContainer(_ context.Context, id string) error {
	name := f.nameOf(id)
	f.log("remove %s", name)
	delete(f.Containers, name)
	return nil
}

func (f *Fake) RemoveVolume(_ context.Context, name string) error {
	f.log("rm-volume %s", name)
	delete(f.Volumes, name)
	return nil
}

func (f *Fake) RemoveNetwork(_ context.Context, name string) error {
	f.log("rm-network %s", name)
	return nil
}

func (f *Fake) LogsTail(context.Context, string, int) (string, error) {
	return "[holder] ASSERT FAIL: example", nil
}

// Lifecycle returns the recorded calls without the network / volume setup.
func (f *Fake) Lifecycle() []string {
	var out []string
	for _, call := range f.Calls {
		if !strings.HasPrefix(call, "network ") && !strings.HasPrefix(call, "volume ") && call != "probe-socket" {
			out = append(out, call)
		}
	}
	return out
}
