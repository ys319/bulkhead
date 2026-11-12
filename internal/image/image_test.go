package image

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"testing/fstest"
)

func source(files map[string]string) Source {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys["holder/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	fsys["dev/Dockerfile"] = &fstest.MapFile{Data: []byte("FROM other")}
	return Source{Repo: "bulkhead-holder", FS: fsys, Dir: "holder"}
}

func TestRefIsStableAndFollowsContent(t *testing.T) {
	base := map[string]string{"Dockerfile": "FROM alpine", "firewall.sh": "#!/bin/sh\n"}
	ref1, err := source(base).Ref()
	if err != nil {
		t.Fatal(err)
	}
	ref2, _ := source(base).Ref()
	if ref1 != ref2 {
		t.Fatalf("same source gave %s and %s", ref1, ref2)
	}
	if !strings.HasPrefix(ref1, "bulkhead-holder:") || len(ref1) != len("bulkhead-holder:")+12 {
		t.Fatalf("unexpected ref %q", ref1)
	}

	changed, _ := source(map[string]string{"Dockerfile": "FROM alpine", "firewall.sh": "#!/bin/sh\necho\n"}).Ref()
	if changed == ref1 {
		t.Fatal("a changed file must change the tag")
	}
	// Moving bytes across a file boundary must not collide.
	moved, _ := source(map[string]string{"Dockerfile": "FROM alpine#", "firewall.sh": "!/bin/sh\n"}).Ref()
	if moved == ref1 {
		t.Fatal("content moved between files must change the tag")
	}
}

func TestWriteContextHoldsOnlyTheSourceDir(t *testing.T) {
	var buf bytes.Buffer
	if err := source(map[string]string{"Dockerfile": "FROM alpine", "firewall.sh": "x"}).WriteContext(&buf); err != nil {
		t.Fatal(err)
	}
	var names []string
	reader := tar.NewReader(&buf)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !header.ModTime.IsZero() && header.ModTime.Unix() != 0 {
			t.Errorf("%s carries a timestamp", header.Name)
		}
		names = append(names, header.Name)
	}
	if got := strings.Join(names, ","); got != "Dockerfile,firewall.sh" {
		t.Fatalf("context holds %q", got)
	}
}

type fakeEngine struct{ present map[string]bool }

func (f fakeEngine) ImageExists(_ context.Context, ref string) (bool, error) {
	return f.present[ref], nil
}

type fakeBuilder struct{ built []string }

func (f *fakeBuilder) Build(_ context.Context, ref string, _ Source, _ io.Writer) error {
	f.built = append(f.built, ref)
	return nil
}

func TestEnsureBuildsOnlyWhenMissing(t *testing.T) {
	src := source(map[string]string{"Dockerfile": "FROM alpine"})
	ref, _ := src.Ref()

	builder := &fakeBuilder{}
	got, err := Ensure(context.Background(), fakeEngine{present: map[string]bool{ref: true}}, builder, src, io.Discard)
	if err != nil || got != ref || len(builder.built) != 0 {
		t.Fatalf("present image: ref=%q err=%v built=%v", got, err, builder.built)
	}

	got, err = Ensure(context.Background(), fakeEngine{}, builder, src, io.Discard)
	if err != nil || got != ref || len(builder.built) != 1 || builder.built[0] != ref {
		t.Fatalf("missing image: ref=%q err=%v built=%v", got, err, builder.built)
	}
}
