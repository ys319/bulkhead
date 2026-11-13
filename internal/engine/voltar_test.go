package engine

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

var homeLeftOut = []string{".cache", ".local/state/nix", ".config/bulkhead/initialized"}

func TestVolumeTarSpecs(t *testing.T) {
	job := VolumeTar{Image: "dev:1", Volume: "bulkhead-x_home", Target: "/home/developer", LeftOut: homeLeftOut}

	config, host := readSpec(job)
	if config.User != "0:0" || !slices.Equal(config.Entrypoint, []string{"tar"}) {
		t.Errorf("read config: %+v", config)
	}
	if host.NetworkMode != "none" || !slices.Equal(host.CapDrop, []string{"ALL"}) || !slices.Equal(host.CapAdd, []string{"DAC_READ_SEARCH"}) ||
		!slices.Equal(host.SecurityOpt, []string{"no-new-privileges:true"}) {
		t.Errorf("read host: %+v", host)
	}
	// Reading cannot change the volume, and mounts it where no image has
	// anything to copy into it.
	if len(host.Mounts) != 1 || !host.Mounts[0].ReadOnly || host.Mounts[0].Source != job.Volume || host.Mounts[0].Target != readMount {
		t.Errorf("read mounts: %+v", host.Mounts)
	}

	config, host = replaceSpec(job)
	if host.NetworkMode != "none" || !slices.Equal(host.CapDrop, []string{"ALL"}) || slices.Contains(host.CapAdd, "SYS_ADMIN") ||
		!slices.Equal(host.SecurityOpt, []string{"no-new-privileges:true"}) || host.Privileged {
		t.Errorf("replace host: %+v", host)
	}
	// Where dev mounts it, so that an empty volume starts from the image.
	if len(host.Mounts) != 1 || host.Mounts[0].ReadOnly || host.Mounts[0].Target != "/home/developer" {
		t.Errorf("replace mounts: %+v", host.Mounts)
	}
	if config.User != "0:0" || !slices.Equal(config.Entrypoint, []string{"sh", "-c"}) {
		t.Errorf("replace config: %+v", config)
	}
}

func TestVolumeTarRefusesWhatCouldLeaveTheVolume(t *testing.T) {
	c := &Client{}
	for _, left := range []string{"../etc", "/etc", "a/../b", "x; echo y", "it's", "a b", "", "./a", "a/", "a*"} {
		job := VolumeTar{Image: "dev:1", Volume: "v", Target: "/home/developer", LeftOut: []string{left}}
		if err := c.ReadVolume(t.Context(), job, io.Discard); err == nil || !strings.Contains(err.Error(), "not a path inside") {
			t.Errorf("read with %q left out: %v", left, err)
		}
		if err := c.ReplaceVolume(t.Context(), job, strings.NewReader("")); err == nil {
			t.Errorf("replace with %q left out went ahead", left)
		}
	}
	for _, target := range []string{"", "/", "relative", "/with space", "/it's"} {
		job := VolumeTar{Image: "dev:1", Volume: "v", Target: target}
		if err := c.ReplaceVolume(t.Context(), job, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "not a place") {
			t.Errorf("replace at %q: %v", target, err)
		}
	}
}

// The script, as text: the order is what keeps it from deleting in the
// wrong place or unpacking over leftovers.
func TestReplaceScriptOrder(t *testing.T) {
	for _, leftOut := range [][]string{nil, homeLeftOut} {
		lines := strings.Split(replaceScript(VolumeTar{Target: "/home/developer", LeftOut: leftOut}), "\n")
		at := func(prefix string) int {
			return slices.IndexFunc(lines, func(line string) bool { return strings.HasPrefix(line, prefix) })
		}
		order := []int{at("set -eu"), at("cd '/home/developer'"), at(`[ "$PWD" = '/home/developer' ]`), at("find . -mindepth 1"), at(`[ -z "$left" ]`), at("exec tar --extract")}
		if order[0] != 0 || !slices.IsSorted(order) || slices.Contains(order, -1) {
			t.Errorf("left out %v: the steps are at %v:\n%s", leftOut, order, strings.Join(lines, "\n"))
		}
		for _, left := range leftOut {
			if !strings.Contains(lines[3], "-path './"+left+"'") {
				t.Errorf("%s is not kept by: %s", left, lines[3])
			}
		}
	}
}

// tree makes files (a name ending in / is a directory) under dir.
func tree(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		path := filepath.Join(dir, name)
		if strings.HasSuffix(name, "/") {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// files lists what is under dir: files and links by name, directories
// with a / at the end.
func files(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || path == dir {
			return err
		}
		name, _ := filepath.Rel(dir, path)
		if info.IsDir() {
			name += "/"
		}
		found = append(found, name)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(found)
	return found
}

func needs(t *testing.T, tools ...string) {
	t.Helper()
	if out, err := exec.Command("tar", "--version").Output(); err != nil || !strings.Contains(string(out), "GNU tar") {
		t.Skip("no GNU tar here")
	}
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s here", tool)
		}
	}
}

// The arguments tar gets in the container, run here on a directory. It
// only reads.
func TestReadLeavesOutOnlyTheNamedPaths(t *testing.T) {
	needs(t)
	dir := t.TempDir()
	tree(t, dir,
		".cache/big", ".cache/deep/er", ".local/state/nix/profile", ".local/state/other", ".local/share/x",
		".config/bulkhead/initialized", ".config/bulkhead/else", ".zshrc",
		// The same names elsewhere are not the left-out paths.
		"project/.cache/keep", "cache", ".cachet", "a*b",
	)
	config, _ := readSpec(VolumeTar{Image: "i", Volume: "v", LeftOut: homeLeftOut})
	args := slices.Clone(config.Cmd)
	for i, arg := range args {
		if arg == "--directory="+readMount {
			args[i] = "--directory=" + dir
		}
	}
	out, err := exec.Command("tar", args...).Output()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	r := tar.NewReader(bytes.NewReader(out))
	for {
		header, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
	sort.Strings(names)
	want := []string{
		"./", "./.cachet", "./.config/", "./.config/bulkhead/", "./.config/bulkhead/else",
		"./.local/", "./.local/share/", "./.local/share/x", "./.local/state/", "./.local/state/other",
		"./.zshrc", "./a*b", "./cache", "./project/", "./project/.cache/", "./project/.cache/keep",
	}
	if !slices.Equal(names, want) {
		t.Errorf("tar read:\n  got  %v\n  want %v", names, want)
	}
}

// The script the container runs, run here on directories the test makes:
// what is left out stays as it is, everything else is replaced by the
// stream. It deletes files, so it runs only when asked for:
//
//	BH_RUN_REPLACE_SCRIPT=1 go test ./internal/engine -run TestReplaceScriptOnDirectories
//
// (scripts/verify-snapshots.sh checks the same in a container, on the
// volumes of a throwaway environment.)
func TestReplaceScriptOnDirectories(t *testing.T) {
	if os.Getenv("BH_RUN_REPLACE_SCRIPT") != "1" {
		t.Skip("deletes files in temporary directories: set BH_RUN_REPLACE_SCRIPT=1 to run it")
	}
	needs(t, "sh", "find")
	run := func(t *testing.T, target string, leftOut []string, stream []byte) (string, error) {
		t.Helper()
		script := exec.Command("sh", "-c", replaceScript(VolumeTar{Target: target, LeftOut: leftOut}))
		script.Stdin = bytes.NewReader(stream)
		out, err := script.CombinedOutput()
		return string(out), err
	}
	pack := func(t *testing.T, names ...string) []byte {
		t.Helper()
		source := t.TempDir()
		tree(t, source, names...)
		out, err := exec.Command("tar", "--create", "--file=-", "--directory="+source, ".").Output()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	t.Run("home", func(t *testing.T) {
		target := t.TempDir()
		tree(t, target,
			".cache/big", ".local/state/nix/profile", ".local/state/other", ".config/bulkhead/initialized",
			".config/bulkhead/else", ".zshrc", "old/deep/file", "empty/", "project/.cache/old", "a*b", "with space/x",
		)
		if err := os.Symlink("/nowhere", filepath.Join(target, "dangling")); err != nil {
			t.Fatal(err)
		}
		if out, err := run(t, target, homeLeftOut, pack(t, ".zshrc", "new/file", ".local/share/y")); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		want := []string{
			".cache/", ".cache/big",
			".config/", ".config/bulkhead/", ".config/bulkhead/initialized",
			".local/", ".local/share/", ".local/share/y", ".local/state/", ".local/state/nix/", ".local/state/nix/profile",
			".zshrc", "new/", "new/file",
		}
		if got := files(t, target); !slices.Equal(got, want) {
			t.Errorf("after the replace:\n  got  %v\n  want %v", got, want)
		}
	})

	t.Run("workspace", func(t *testing.T) {
		target := t.TempDir()
		tree(t, target, ".git/config", "src/main.go", ".hidden", "-dash", "empty/")
		if out, err := run(t, target, nil, pack(t, "src/new.go")); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if got, want := files(t, target), []string{"src/", "src/new.go"}; !slices.Equal(got, want) {
			t.Errorf("after the replace:\n  got  %v\n  want %v", got, want)
		}
	})

	t.Run("a place that is not there", func(t *testing.T) {
		if out, err := run(t, filepath.Join(t.TempDir(), "missing"), nil, pack(t, "x")); err == nil {
			t.Errorf("went ahead:\n%s", out)
		}
	})
}
