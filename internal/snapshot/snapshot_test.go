package snapshot

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// entry is one member of a volume's tar stream.
type entry struct {
	name, link string
	kind       byte
	data       string
	uid        int
	mode       int64
}

func stream(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	w := tar.NewWriter(&buffer)
	for _, e := range entries {
		header := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: e.kind, Uid: e.uid, Gid: e.uid, Mode: e.mode, Size: int64(len(e.data)), Format: tar.FormatGNU}
		if err := w.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func entriesOf(t *testing.T, in io.Reader) []entry {
	t.Helper()
	var entries []entry
	r := tar.NewReader(in)
	for {
		header, err := r.Next()
		if errors.Is(err, io.EOF) {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry{name: header.Name, link: header.Linkname, kind: header.Typeflag, data: string(data), uid: header.Uid, mode: header.Mode})
	}
}

var (
	workspace = []entry{
		{name: "./", kind: tar.TypeDir, uid: 1000, mode: 0o755},
		{name: "./src/", kind: tar.TypeDir, uid: 1000, mode: 0o755},
		{name: "./src/main.go", kind: tar.TypeReg, data: "package main\n", uid: 1000, mode: 0o644},
		{name: "./src/a file", kind: tar.TypeReg, data: "x", uid: 1000, mode: 0o600},
		{name: "./src/other", kind: tar.TypeLink, link: "./src/main.go", uid: 1000, mode: 0o644},
		{name: "./dangling", kind: tar.TypeSymlink, link: "/nix/store/gone", uid: 1000, mode: 0o777},
		{name: "./" + strings.Repeat("deep/", 40) + "leaf", kind: tar.TypeReg, data: "long name", uid: 1000, mode: 0o644},
	}
	home = []entry{
		{name: "./", kind: tar.TypeDir, uid: 1000, mode: 0o700},
		{name: "./.zshrc", kind: tar.TypeReg, data: "export A=1\n", uid: 1000, mode: 0o644},
		{name: "./root-made", kind: tar.TypeReg, data: "r", uid: 0, mode: 0o644},
	}
	manifest = Manifest{
		Kind: KindManual, Environment: Environment{ID: "0123456789ab", Name: "web"},
		Created: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC), BH: "test", Engine: "linux/arm64",
		Volumes: []Volume{{Key: "workspace"}, {Key: "home", LeftOut: []string{".cache", ".local/state/nix"}}},
	}
)

func write(t *testing.T, path string, dockerfile []byte) Summary {
	t.Helper()
	w, err := Create(path, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if dockerfile != nil {
		if err := w.AddDockerfile(dockerfile); err != nil {
			t.Fatal(err)
		}
	}
	for key, entries := range map[string][]entry{"workspace": workspace, "home": home} {
		_ = key
		_ = entries
	}
	for _, part := range []struct {
		key     string
		entries []entry
	}{{"workspace", workspace}, {"home", home}} {
		data := stream(t, part.entries)
		if _, err := w.AddVolume(part.key, func(out io.Writer) error { _, err := out.Write(data); return err }); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	return summary
}

func TestWhatGoesInComesOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "one"+Extension)
	summary := write(t, path, []byte("FROM bulkhead-dev-base\n"))
	if got := summary.Volumes["workspace"]; got.Entries != len(workspace) || got.Bytes != int64(len("package main\n")+1+len("long name")) {
		t.Errorf("workspace summary = %+v", got)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the file: %v %v", info, err)
	}
	if _, err := os.Stat(path + ".partial"); err == nil {
		t.Error("the partial file stayed")
	}

	read, err := ReadManifest(path)
	if err != nil || read.Environment.Name != "web" || read.Format != Format || len(read.Volumes) != 2 {
		t.Fatalf("manifest = %+v, %v", read, err)
	}
	checked, err := Verify(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(checked.Dockerfile) != "FROM bulkhead-dev-base\n" || checked.Summary.Bytes() != summary.Bytes() {
		t.Errorf("verified: %+v", checked)
	}

	got := map[string][]entry{}
	var order []string
	_, err = Restore(path, nil, func(volume Volume, in io.Reader) error {
		order = append(order, volume.Key)
		got[volume.Key] = entriesOf(t, in)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "workspace,home" {
		t.Errorf("order = %v", order)
	}
	for key, want := range map[string][]entry{"workspace": workspace, "home": home} {
		if len(got[key]) != len(want) {
			t.Fatalf("%s: %d entries, want %d", key, len(got[key]), len(want))
		}
		for i := range want {
			if got[key][i] != want[i] {
				t.Errorf("%s entry %d:\n  got  %+v\n  want %+v", key, i, got[key][i], want[i])
			}
		}
	}

	// One volume only: the other is read past, not handed over.
	order = nil
	if _, err := Restore(path, []string{"home"}, func(volume Volume, in io.Reader) error {
		order = append(order, volume.Key)
		_, err := io.Copy(io.Discard, in)
		return err
	}); err != nil || strings.Join(order, ",") != "home" {
		t.Errorf("home only: %v, %v", order, err)
	}
}

func TestWithoutADockerfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "one"+Extension)
	write(t, path, nil)
	checked, err := Verify(path)
	if err != nil || checked.Dockerfile != nil {
		t.Errorf("verify: %q, %v", checked.Dockerfile, err)
	}
}

func TestADamagedFileDoesNotPass(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "one"+Extension)
	write(t, path, nil)
	whole, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	damaged := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	for _, cut := range []int{len(whole) - 1, len(whole) - 20, len(whole) / 2, 40} {
		if _, err := Verify(damaged("cut", whole[:cut])); err == nil {
			t.Errorf("a file cut at %d of %d passed", cut, len(whole))
		}
	}
	flipped := bytes.Clone(whole)
	flipped[len(flipped)/2] ^= 0xff
	if _, err := Verify(damaged("flipped", flipped)); err == nil {
		t.Error("a file with a changed byte passed")
	}
	if _, err := Verify(damaged("other", []byte("not a snapshot"))); err == nil {
		t.Error("another kind of file passed")
	}
	// A writer that stops early stops the restore with its error.
	stop := errors.New("disk full")
	if _, err := Restore(path, nil, func(Volume, io.Reader) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("restore error = %v", err)
	}
}

func TestAnInterruptedWriteLeavesNoSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "one"+Extension)
	w, err := Create(path, manifest)
	if err != nil {
		t.Fatal(err)
	}
	broken := errors.New("the container died")
	if _, err := w.AddVolume("workspace", func(out io.Writer) error {
		out.Write(stream(t, workspace)[:700])
		return broken
	}); !errors.Is(err, broken) {
		t.Errorf("AddVolume error = %v", err)
	}
	w.Abort()
	for _, name := range []string{path, path + ".partial"} {
		if _, err := os.Stat(name); err == nil {
			t.Errorf("%s is there", name)
		}
	}

	// A volume that was never added does not make a snapshot either.
	w, err = Create(path, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.AddVolume("workspace", func(out io.Writer) error { _, err := out.Write(stream(t, workspace)); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err == nil {
		t.Error("closed without the home volume")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the incomplete file got its name")
	}
}

func TestNamesThatLeaveTheVolumeAreRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "one"+Extension)
	w, err := Create(path, manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Abort()
	escape := stream(t, []entry{{name: "../../etc/passwd", kind: tar.TypeReg, data: "x"}})
	if _, err := w.AddVolume("workspace", func(out io.Writer) error { _, err := out.Write(escape); return err }); err == nil {
		t.Error("an entry outside the volume went in")
	}
}

func TestManifestIsChecked(t *testing.T) {
	bad := []Manifest{
		{Format: Format},
		{Format: Format, Volumes: []Volume{{Key: "home"}, {Key: "home"}}},
		{Format: Format, Volumes: []Volume{{Key: "../x"}}},
		{Format: Format, Volumes: []Volume{{Key: "home", LeftOut: []string{"../etc"}}}},
		{Format: Format, Volumes: []Volume{{Key: "home", LeftOut: []string{"a; rm -rf /"}}}},
		{Format: Format, Volumes: []Volume{{Key: "home", LeftOut: []string{"/abs"}}}},
		{Format: Format, Volumes: []Volume{{Key: "home", LeftOut: []string{"'quote"}}}},
		{Format: Format + 1, Volumes: []Volume{{Key: "home"}}},
	}
	for i, m := range bad {
		if err := m.check(); err == nil {
			t.Errorf("manifest %d passed: %+v", i, m)
		}
	}
	if err := manifest.withFormat().check(); err != nil {
		t.Errorf("the good manifest: %v", err)
	}
}

func (m Manifest) withFormat() Manifest { m.Format = Format; return m }

// What the volumes really come as is the output of GNU tar, and what they
// go back through is GNU tar again.
func TestThroughRealTar(t *testing.T) {
	if out, err := exec.Command("tar", "--version").Output(); err != nil || !strings.Contains(string(out), "GNU tar") {
		t.Skip("no GNU tar here")
	}
	source, target := t.TempDir(), t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	deep := filepath.Join(source, strings.Repeat("a-rather-long-directory-name/", 8))
	must(os.MkdirAll(deep, 0o755))
	must(os.MkdirAll(filepath.Join(source, "private"), 0o700))
	must(os.WriteFile(filepath.Join(deep, "leaf with space"), []byte("leaf"), 0o644))
	must(os.WriteFile(filepath.Join(source, "private", "token"), []byte("secret"), 0o600))
	must(os.WriteFile(filepath.Join(source, "one"), bytes.Repeat([]byte("0123456789"), 100_000), 0o644))
	must(os.Link(filepath.Join(source, "one"), filepath.Join(source, "two")))
	must(os.Symlink("/nix/store/gone", filepath.Join(source, "dangling")))
	must(os.WriteFile(filepath.Join(source, "ユニコード"), []byte("u"), 0o644))

	path := filepath.Join(t.TempDir(), "real"+Extension)
	w, err := Create(path, Manifest{Kind: KindManual, Created: time.Now(), Volumes: []Volume{{Key: "workspace"}}})
	must(err)
	_, err = w.AddVolume("workspace", func(out io.Writer) error {
		pack := exec.Command("tar", "--create", "--file=-", "--directory="+source, "--numeric-owner", ".")
		pack.Stdout = out
		return pack.Run()
	})
	must(err)
	_, err = w.Close()
	must(err)

	_, err = Verify(path)
	must(err)
	_, err = Restore(path, nil, func(_ Volume, in io.Reader) error {
		unpack := exec.Command("tar", "--extract", "--file=-", "--directory="+target, "--numeric-owner")
		unpack.Stdin = in
		output, err := unpack.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, output)
		}
		return nil
	})
	must(err)

	if output, err := exec.Command("diff", "-r", "--no-dereference", source, target).CombinedOutput(); err != nil {
		t.Errorf("the trees differ: %v\n%s", err, output)
	}
	one, err := os.Stat(filepath.Join(target, "one"))
	must(err)
	two, err := os.Stat(filepath.Join(target, "two"))
	must(err)
	if !os.SameFile(one, two) {
		t.Error("the two names of one file came back as two files")
	}
	if info, err := os.Stat(filepath.Join(target, "private")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("private: %v %v", info, err)
	}
	if link, err := os.Readlink(filepath.Join(target, "dangling")); err != nil || link != "/nix/store/gone" {
		t.Errorf("dangling: %q %v", link, err)
	}
}
