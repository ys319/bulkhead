package ops

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/ys319/bulkhead/internal/engine/enginetest"
)

const (
	oldHolder = "bulkhead-e1-holder-1"
	oldDev    = "bulkhead-e1-dev-1"
)

// volumesUntouched fails the test if any volume was removed or any call to
// remove one was made. This is the property migration exists to keep.
func volumesUntouched(t *testing.T, f *enginetest.Fake, before map[string]bool) {
	t.Helper()
	for name := range before {
		if !f.Volumes[name] {
			t.Errorf("the volume %s is gone", name)
		}
	}
	for _, call := range f.Calls {
		if strings.HasPrefix(call, "rm-volume") {
			t.Errorf("a volume was removed: %s", call)
		}
	}
}

// referenced fails the test unless, after every single step, some container
// of the environment existed — the volumes must never look unused.
func alwaysReferenced(t *testing.T, calls []string) {
	t.Helper()
	alive := map[string]bool{oldHolder: true, oldDev: true}
	for _, call := range calls {
		verb, name, _ := strings.Cut(call, " ")
		switch verb {
		case "create":
			alive[name] = true
		case "remove":
			delete(alive, name)
		}
		// dev is the one that mounts them.
		if !alive[oldDev] && !alive[devName] {
			t.Fatalf("after %q no dev container refers to the volumes (calls: %v)", call, calls)
		}
	}
}

func TestAdoptReplacesARunningEnvironment(t *testing.T) {
	f := enginetest.New()
	f.AddLegacy("e1", true)
	before := maps.Clone(f.Volumes)

	adoption, err := runner(f).Adopt(context.Background(), env, target)
	if err != nil {
		t.Fatal(err)
	}
	if !adoption.Started || len(adoption.Leftovers) != 0 {
		t.Errorf("adoption = %+v", adoption)
	}
	// New containers first; the old ones stop to free the port, and go only
	// after the new ones are up behind a verified filter.
	expectCalls(t, f,
		"create "+holderName, "create "+devName,
		"stop "+oldDev, "stop "+oldHolder,
		"start "+holderName, "start "+devName,
		"remove "+oldDev, "remove "+oldHolder,
		"rm-network bulkhead-e1_default")
	alwaysReferenced(t, f.Lifecycle())
	volumesUntouched(t, f, before)
	if dev := f.Containers[devName]; dev.NetworkMode != "container:"+f.Containers[holderName].ID {
		t.Errorf("the new dev is joined to %q", dev.NetworkMode)
	}
}

func TestAdoptLeavesAStoppedEnvironmentStopped(t *testing.T) {
	f := enginetest.New()
	f.AddLegacy("e1", false)
	before := maps.Clone(f.Volumes)

	adoption, err := runner(f).Adopt(context.Background(), env, target)
	if err != nil {
		t.Fatal(err)
	}
	if adoption.Started {
		t.Error("a stopped environment was started")
	}
	// The new containers exist (created, not started), so the volumes are
	// still referred to once the old ones are gone.
	expectCalls(t, f,
		"create "+holderName, "create "+devName,
		"remove "+oldDev, "remove "+oldHolder,
		"rm-network bulkhead-e1_default")
	alwaysReferenced(t, f.Lifecycle())
	volumesUntouched(t, f, before)

	// And a later up starts exactly those.
	f.Calls = nil
	if err := runner(f).Up(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	expectCalls(t, f, "start "+holderName, "start "+devName)
}

// The new holder does not get healthy: back to the old containers, as if
// nothing had been tried.
func TestAdoptGoesBackWhenTheNewContainersDoNotComeUp(t *testing.T) {
	f := enginetest.New()
	f.AddLegacy("e1", true)
	f.ExitOnStart, f.ExitOnStartName = true, holderName
	before := maps.Clone(f.Volumes)

	_, err := runner(f).Adopt(context.Background(), env, target)

	var gate *GateError
	if !errors.As(err, &gate) || !strings.Contains(err.Error(), "old containers were started again") {
		t.Fatalf("err = %v", err)
	}
	if _, exists := f.Containers[holderName]; exists {
		t.Error("the new holder was left behind")
	}
	if _, exists := f.Containers[devName]; exists {
		t.Error("the new dev was left behind")
	}
	for _, name := range []string{oldHolder, oldDev} {
		if c := f.Containers[name]; c == nil || c.Status != "running" {
			t.Errorf("%s is not running again: %+v", name, c)
		}
	}
	calls := f.Lifecycle()
	// The old dev comes back only after the old holder.
	if h, d := slices.Index(calls, "start "+oldHolder), slices.Index(calls, "start "+oldDev); h < 0 || d < h {
		t.Errorf("the old containers were not restarted holder first: %v", calls)
	}
	if slices.Contains(calls, "start "+devName) {
		t.Errorf("the new dev was started past a closed gate: %v", calls)
	}
	alwaysReferenced(t, calls)
	volumesUntouched(t, f, before)

	// It can simply be tried again.
	f.ExitOnStart = false
	if _, err := runner(f).Adopt(context.Background(), env, target); err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	volumesUntouched(t, f, before)
}

// Adopt stops and removes the old containers, so it must be sure they are
// what it thinks they are.
func TestAdoptRefusesAContainerComposeDidNotMake(t *testing.T) {
	f := enginetest.New()
	f.AddLegacy("e1", true)
	f.Containers[oldDev].Labels = map[string]string{"com.docker.compose.project": "something-else"}

	_, err := runner(f).Adopt(context.Background(), env, target)
	if err == nil || !strings.Contains(err.Error(), "not touching it") {
		t.Fatalf("err = %v", err)
	}
	if calls := f.Lifecycle(); len(calls) != 0 {
		t.Errorf("something was done: %v", calls)
	}
}

// Interrupted after the new containers came up (the old ones already gone):
// running it again changes nothing and still succeeds.
func TestAdoptCanBeRunAgain(t *testing.T) {
	f := enginetest.New()
	f.AddLegacy("e1", true)
	r := runner(f)
	if _, err := r.Adopt(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	before := maps.Clone(f.Volumes)
	f.Calls = nil

	adoption, err := r.Adopt(context.Background(), env, target)
	if err != nil || !adoption.Started {
		t.Fatalf("adoption = %+v, %v", adoption, err)
	}
	expectCalls(t, f, "rm-network bulkhead-e1_default")
	volumesUntouched(t, f, before)
}

func TestReleaseKeepsTheVolumes(t *testing.T) {
	f := enginetest.New()
	r := runner(f)
	if err := r.Up(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	before := maps.Clone(f.Volumes)
	f.Calls = nil

	if err := r.Release(context.Background(), env.ID); err != nil {
		t.Fatal(err)
	}
	expectCalls(t, f,
		"stop "+devName, "stop "+holderName,
		"remove "+devName, "remove "+holderName,
		"rm-network bulkhead-e1")
	if len(before) != 4 {
		t.Fatalf("volumes before: %v", before)
	}
	volumesUntouched(t, f, before)
}
