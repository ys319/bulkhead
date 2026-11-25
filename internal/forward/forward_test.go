package forward

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The sides never change places: this machine's port left, the
// environment's right, as Docker writes a published port. host: says
// whose service it is.
func TestPortsAreWrittenAsDockerWritesThem(t *testing.T) {
	for text, want := range map[string]Port{
		"3000":         {Host: 3000, Env: 3000},
		"8080:80":      {Host: 8080, Env: 80},
		"host:8000":    {Host: 8000, Env: 8000, FromHost: true},
		"host:80:8080": {Host: 80, Env: 8080, FromHost: true},
	} {
		got, err := Parse(text)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", text, got, err, want)
		}
		if got.String() != text {
			t.Errorf("%q is written back as %q", text, got.String())
		}
		if !IsPort(text) {
			t.Errorf("IsPort(%q) = false", text)
		}
	}
	// The shortest form is the one written.
	if port, _ := Parse("3000:3000"); port.String() != "3000" {
		t.Errorf("3000:3000 is written back as %q", port)
	}
	for _, text := range []string{"", "0", "65536", "80:0", "web", "env:80", "host:", "host", "80:81:82", "-80", "80 ", "8080>80", "localhost:80", "HOST:80"} {
		if _, err := Parse(text); err == nil {
			t.Errorf("Parse(%q) passed", text)
		}
	}
	// A port out of range is still written like a port: it is a mistake in
	// a port, not a name.
	if !IsPort("99999") || IsPort("api") || IsPort("host") {
		t.Error("IsPort does not tell ports from names")
	}
}

func TestDescribeSaysWhichWayConnectionsGo(t *testing.T) {
	if got := (Port{Host: 8080, Env: 80}).Describe(); got != "this machine's localhost:8080 → the environment's port 80" {
		t.Errorf("out of the environment: %q", got)
	}
	if got := (Port{Host: 80, Env: 8080, FromHost: true}).Describe(); got != "the environment's localhost:8080 → this machine's port 80" {
		t.Errorf("into the environment: %q", got)
	}
}

// Both ends listen on loopback only, and a forward from this machine leads
// to this machine itself and nowhere else on its networks.
func TestSSHArgsListenOnLoopbackOnly(t *testing.T) {
	got := SSHArgs([]Port{{Host: 8080, Env: 80}, {Host: 8000, Env: 8000, FromHost: true}, {Host: 80, Env: 8080, FromHost: true}})
	want := []string{
		"-L", "127.0.0.1:8080:127.0.0.1:80",
		"-R", "127.0.0.1:8000:localhost:8000",
		"-R", "127.0.0.1:8080:localhost:80",
	}
	if !slices.Equal(got, want) {
		t.Errorf("SSHArgs = %q", got)
	}
	// A connection that could not listen on a port ends instead of going
	// on without it, and never asks at a terminal nobody is at.
	options := SessionOptions()
	for _, option := range []string{"ExitOnForwardFailure=yes", "BatchMode=yes"} {
		if !slices.Contains(options, option) {
			t.Errorf("no %s among %q", option, options)
		}
	}
}

func TestPortsThatCannotBeOpenTogetherAreRefused(t *testing.T) {
	for _, words := range [][]string{
		{"3000", "3000:80"},           // this machine's 3000 twice
		{"host:80:8080", "host:8080"}, // the environment's 8080 twice
	} {
		if _, err := ParseAll(words); err == nil || !strings.Contains(err.Error(), "both listen on port") {
			t.Errorf("%q: %v", words, err)
		}
	}
	// The same number on the two sides is two different ports, and one
	// service may be reached at two.
	for _, words := range [][]string{{"3000", "host:3000"}, {"8080:80", "8081:80"}, {"host:5432:1", "host:5432:2"}} {
		if _, err := ParseAll(words); err != nil {
			t.Errorf("%q: %v", words, err)
		}
	}
}

func TestSetsHaveNamesThatAreNoPorts(t *testing.T) {
	for _, name := range []string{"api", "a", "web-2"} {
		if err := CheckName(name); err != nil {
			t.Errorf("CheckName(%q): %v", name, err)
		}
	}
	for _, name := range []string{"", "3000", "2web", "Web", "host", "a b", "-a", strings.Repeat("a", 33)} {
		if err := CheckName(name); err == nil {
			t.Errorf("CheckName(%q) passed", name)
		}
	}
	port := []Port{{Host: 1, Env: 1}}
	for name, sets := range map[string][]Set{
		"the same name twice": {{Name: "a", Ports: port}, {Name: "a", Ports: port}},
		"no ports":            {{Name: "a"}},
		"a clash within":      {{Name: "a", Ports: []Port{{Host: 1, Env: 1}, {Host: 1, Env: 2}}}},
	} {
		if err := CheckSets(sets); err == nil {
			t.Errorf("%s passed", name)
		}
	}
	// Two sets may want the same port: only one of them may be open then.
	if err := CheckSets([]Set{{Name: "a", Ports: port}, {Name: "b", Ports: port}}); err != nil {
		t.Errorf("two sets with one port: %v", err)
	}
}

// One holder at a time, and the lock is the truth about who it is.
func TestTheLockIsHeldByOneAtATime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "id", "forward-a.lock")
	if _, held := Holder(path); held {
		t.Error("held before anybody took it")
	}
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path); !errors.Is(err, ErrHeld) {
		t.Errorf("a second hold: %v", err)
	}
	if pid, held := Holder(path); !held || pid <= 0 {
		t.Errorf("Holder = %d, %v", pid, held)
	}
	first.Release()
	first.Release() // twice is once
	if pid, held := Holder(path); held || pid != 0 {
		t.Errorf("after the release: %d, %v", pid, held)
	}
	second, err := Acquire(path)
	if err != nil {
		t.Fatalf("after the release: %v", err)
	}
	second.Release()
}
