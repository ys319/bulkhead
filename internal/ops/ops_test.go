package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ys319/bulkhead/internal/engine/enginetest"
	"github.com/ys319/bulkhead/internal/plan"
)

var (
	env    = plan.Env{ID: "e1", Name: "main", SSHPort: 22001, Restart: "no"}
	target = plan.Target{Owner: "cfg", HolderImage: "holder:1", DevImage: "dev:1"}
)

const (
	holderName = "bulkhead-e1-holder"
	devName    = "bulkhead-e1-dev"
)

func runner(f *enginetest.Fake) Runner {
	return Runner{Engine: f, Sleep: func(time.Duration) {}, GateTimeout: 3 * gatePoll}
}

func expectCalls(t *testing.T, f *enginetest.Fake, want ...string) {
	t.Helper()
	if got := f.Lifecycle(); !slices.Equal(got, want) {
		t.Fatalf("calls:\n  got  %v\n  want %v", got, want)
	}
}

func TestUpStartsDevOnlyAfterTheHolderIsHealthy(t *testing.T) {
	f := enginetest.New()
	f.HealthAfterStart = []string{"starting", "starting", "healthy"}

	if err := runner(f).Up(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	expectCalls(t, f, "create "+holderName, "start "+holderName, "create "+devName, "start "+devName)

	dev := f.Containers[devName]
	if dev.NetworkMode != "container:"+f.Containers[holderName].ID {
		t.Errorf("dev NetworkMode = %q, want the holder's id", dev.NetworkMode)
	}
	if !slices.Contains(f.Calls, "network bulkhead-e1") || !slices.Contains(f.Calls, "volume bulkhead-e1_workspace") {
		t.Errorf("network / volumes not prepared: %v", f.Calls)
	}
}

// The gate: whatever goes wrong with the holder, dev must not come up.
func TestUpNeverStartsDevPastAClosedGate(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*enginetest.Fake)
		reason string
	}{
		{"holder turns unhealthy", func(f *enginetest.Fake) { f.HealthAfterStart = []string{"starting", "unhealthy"} }, "unhealthy"},
		{"holder exits (self-assert failed)", func(f *enginetest.Fake) { f.ExitOnStart = true }, "it exited"},
		{"holder never gets healthy", func(f *enginetest.Fake) { f.HealthAfterStart = []string{"starting"} }, "timed out"},
		{"holder has no healthcheck", func(f *enginetest.Fake) { f.HealthAfterStart = []string{""} }, "no healthcheck"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := enginetest.New()
			tt.setup(f)

			err := runner(f).Up(context.Background(), env, target)

			var gate *GateError
			if !errors.As(err, &gate) || !strings.Contains(gate.Reason, tt.reason) {
				t.Fatalf("err = %v, want a GateError about %q", err, tt.reason)
			}
			if !strings.Contains(err.Error(), "ASSERT FAIL") {
				t.Errorf("the error should carry the holder's log tail: %v", err)
			}
			if _, exists := f.Containers[devName]; exists {
				t.Error("dev was created past a closed gate")
			}
			// The holder is stopped, not left to its restart policy.
			expectCalls(t, f, "create "+holderName, "start "+holderName, "stop "+holderName)
		})
	}
}

func TestUpIsANoOpWhenEverythingIsUp(t *testing.T) {
	f := enginetest.New()
	r := runner(f)
	if err := r.Up(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	f.Calls = nil

	if err := r.Up(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	expectCalls(t, f)
}

func TestUpAfterDownRestartsTheSameContainers(t *testing.T) {
	f := enginetest.New()
	r := runner(f)
	if err := r.Up(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	if err := r.Down(context.Background(), env.ID); err != nil {
		t.Fatal(err)
	}
	f.Calls = nil

	if err := r.Up(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	// No create: stopped containers are reused, holder first.
	expectCalls(t, f, "start "+holderName, "start "+devName)
}

func TestUpRecreatesBothWhenTheHolderSettingsChange(t *testing.T) {
	f := enginetest.New()
	r := runner(f)
	if err := r.Up(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	f.Calls = nil

	changed := env
	changed.SSHPort = 22002
	if err := r.Up(context.Background(), changed, target); err != nil {
		t.Fatal(err)
	}
	expectCalls(t, f,
		"remove "+devName, "remove "+holderName,
		"create "+holderName, "start "+holderName,
		"create "+devName, "start "+devName)
}

func TestUpRecreatesOnlyDevWhenOnlyDevChanges(t *testing.T) {
	f := enginetest.New()
	r := runner(f)
	if err := r.Up(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	f.Calls = nil

	changed := env
	changed.AuthorizedKeys = []string{"ssh-ed25519 AAAA new"}
	if err := r.Up(context.Background(), changed, target); err != nil {
		t.Fatal(err)
	}
	expectCalls(t, f, "remove "+devName, "create "+devName, "start "+devName)
}

// A holder that stopped while dev kept running: dev sits in a dead namespace.
func TestUpMovesARunningDevToTheFreshNamespace(t *testing.T) {
	f := enginetest.New()
	r := runner(f)
	if err := r.Up(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	f.Containers[holderName].Status = "exited"
	f.Containers[holderName].Health = ""
	f.Calls = nil

	if err := r.Up(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	expectCalls(t, f, "stop "+devName, "start "+holderName, "start "+devName)
}

// A holder that restarted on its own (crash, `docker restart`) while dev
// kept running: both are "running", but dev is in the previous namespace.
func TestUpRestartsADevOlderThanTheHolder(t *testing.T) {
	f := enginetest.New()
	r := runner(f)
	if err := r.Up(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	// The holder restarts behind our back and has become healthy again.
	f.Clock = f.Clock.Add(time.Minute)
	f.Containers[holderName].StartedAt = f.Clock
	f.Calls = nil

	if err := r.Up(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	expectCalls(t, f, "stop "+devName, "start "+devName)

	// And then it is settled.
	f.Calls = nil
	if err := r.Up(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	expectCalls(t, f)
}

// A dev left over from a previous holder (different container id) must not
// be started as it is.
func TestUpReplacesADevThatPointsAtAnotherHolder(t *testing.T) {
	f := enginetest.New()
	r := runner(f)
	if err := r.Up(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	if err := r.Down(context.Background(), env.ID); err != nil {
		t.Fatal(err)
	}
	f.Containers[devName].NetworkMode = "container:some-old-holder"
	f.Calls = nil

	if err := r.Up(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	expectCalls(t, f, "start "+holderName, "remove "+devName, "create "+devName, "start "+devName)
}

func TestDownStopsDevFirstAndKeepsTheContainers(t *testing.T) {
	f := enginetest.New()
	r := runner(f)
	if err := r.Up(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	f.Calls = nil

	if err := r.Down(context.Background(), env.ID); err != nil {
		t.Fatal(err)
	}
	expectCalls(t, f, "stop "+devName, "stop "+holderName)
	if len(f.Containers) != 2 {
		t.Errorf("containers after down: %d, want both kept", len(f.Containers))
	}

	// Down again: nothing to do.
	f.Calls = nil
	if err := r.Down(context.Background(), env.ID); err != nil {
		t.Fatal(err)
	}
	expectCalls(t, f)
}

func TestDestroyRemovesContainersVolumesAndNetwork(t *testing.T) {
	f := enginetest.New()
	r := runner(f)
	if err := r.Up(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	f.Calls = nil

	if err := r.Destroy(context.Background(), env.ID); err != nil {
		t.Fatal(err)
	}
	expectCalls(t, f,
		"remove "+devName, "remove "+holderName,
		"rm-volume bulkhead-e1_workspace", "rm-volume bulkhead-e1_home",
		"rm-volume bulkhead-e1_nix", "rm-volume bulkhead-e1_hostkeys",
		"rm-volume bulkhead-e1_github", "rm-volume bulkhead-e1_tokens",
		"rm-network bulkhead-e1")
}

func TestRestartRecreatesBothContainers(t *testing.T) {
	f := enginetest.New()
	r := runner(f)
	if err := r.Up(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	f.Calls = nil

	if err := r.Restart(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	expectCalls(t, f,
		"stop "+devName, "stop "+holderName,
		"remove "+devName, "remove "+holderName,
		"create "+holderName, "start "+holderName,
		"create "+devName, "start "+devName)
}

func TestState(t *testing.T) {
	ctx := context.Background()
	f := enginetest.New()
	r := runner(f)
	state := func() State {
		t.Helper()
		status, err := r.Inspect(ctx, env.ID)
		if err != nil {
			t.Fatal(err)
		}
		return status.State()
	}
	expect := func(want State, when string) {
		t.Helper()
		if got := state(); got != want {
			t.Errorf("%s: state = %q, want %q", when, got, want)
		}
	}

	expect(StateAbsent, "before the first up")
	if err := r.Up(ctx, env, target); err != nil {
		t.Fatal(err)
	}
	expect(StateRunning, "after up")
	if err := r.Down(ctx, env.ID); err != nil {
		t.Fatal(err)
	}
	expect(StateStopped, "after down")
	if err := r.Up(ctx, env, target); err != nil {
		t.Fatal(err)
	}

	// Each of these looks alive in `docker ps` and is not a working environment.
	f.Containers[devName].Status = "exited"
	expect(StateDegraded, "dev down, holder up")
	f.Containers[devName].Status = "running"

	f.HealthAfterStart = []string{"unhealthy"} // what the fake reports while not healthy
	f.Containers[holderName].Health = "unhealthy"
	expect(StateDegraded, "holder unhealthy")
	f.Containers[holderName].Health = "healthy"

	f.Containers[holderName].StartedAt = f.Clock.Add(time.Minute)
	expect(StateDegraded, "holder restarted alone (dev in the old namespace)")
	f.Containers[holderName].StartedAt = f.Containers[devName].StartedAt.Add(-time.Second)

	f.Containers[devName].NetworkMode = "container:another"
	expect(StateDegraded, "dev joined to another holder")
	f.Containers[devName].NetworkMode = "container:" + f.Containers[holderName].ID

	expect(StateRunning, "restored")

	// The holder fails its self-assert over and over under a restart policy.
	f.Containers[holderName].Status = "restarting"
	expect(StateDegraded, "holder in a restart loop")
	f.Containers[holderName].Status = "running"

	// The holder is up and still proving its filter; dev waits.
	f.Containers[devName].Status = "exited"
	f.Containers[holderName].Health = "starting"
	f.HealthAfterStart = []string{"starting"}
	expect(StateStarting, "during up")
}

func TestResetRemovesHomeAndNixOnly(t *testing.T) {
	f := enginetest.New()
	r := runner(f)
	if err := r.Up(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	f.Calls = nil

	if err := r.Reset(context.Background(), env.ID); err != nil {
		t.Fatal(err)
	}
	expectCalls(t, f,
		"stop "+devName, "stop "+holderName,
		"remove "+devName, "remove "+holderName,
		"rm-volume bulkhead-e1_home", "rm-volume bulkhead-e1_nix")
	// The user's work, the ssh identity and the credentials stay.
	for _, key := range []string{"workspace", "hostkeys", "github", "tokens"} {
		if !f.Volumes["bulkhead-e1_"+key] {
			t.Errorf("the %s volume is gone", key)
		}
	}

	// The next up starts from fresh volumes.
	if err := r.Up(context.Background(), env, target); err != nil {
		t.Fatal(err)
	}
	if !f.Volumes["bulkhead-e1_home"] || !f.Volumes["bulkhead-e1_nix"] {
		t.Error("up did not create the volumes again")
	}
}

func TestOnlyDestroyAndResetRemoveVolumes(t *testing.T) {
	// The rule the project is built around, checked on the source: the
	// engine's RemoveVolume is called from removeVolumes, and removeVolumes
	// from Destroy and Reset.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var removers, callers int
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		removers += strings.Count(string(data), ".RemoveVolume(ctx")
		callers += strings.Count(string(data), "r.removeVolumes(")
	}
	if removers != 1 || callers != 2 {
		t.Errorf("RemoveVolume is called from %d places (want 1), removeVolumes from %d (want 2: Destroy and Reset)", removers, callers)
	}
}
