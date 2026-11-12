// Package image turns an embedded source directory into a Docker image.
package image

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
)

// Source is a build context inside a file system (the embedded containers/).
type Source struct {
	Repo string // image repository, e.g. "bulkhead-holder"
	FS   fs.FS
	Dir  string
}

// Ref is the image reference for this exact source: the tag is a hash of the
// files, so a newer bh builds a new image instead of silently reusing a
// stale one, and "was this image built from this binary" is visible by name.
func (s Source) Ref() (string, error) {
	sum := sha256.New()
	err := s.walk(func(name string, data []byte) error {
		fmt.Fprintf(sum, "%s\x00%d\x00", name, len(data))
		sum.Write(data)
		return nil
	})
	if err != nil {
		return "", err
	}
	return s.Repo + ":" + hex.EncodeToString(sum.Sum(nil))[:12], nil
}

// WriteContext writes the build context as a tar stream. Headers carry no
// timestamps or owners, so the same source always yields the same bytes.
func (s Source) WriteContext(w io.Writer) error {
	tw := tar.NewWriter(w)
	err := s.walk(func(name string, data []byte) error {
		header := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// walk visits the regular files under Dir in lexical order, with paths
// relative to Dir.
func (s Source) walk(visit func(name string, data []byte) error) error {
	root, err := fs.Sub(s.FS, s.Dir)
	if err != nil {
		return err
	}
	return fs.WalkDir(root, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := fs.ReadFile(root, path)
		if err != nil {
			return err
		}
		return visit(path, data)
	})
}

// Builder builds an image from a source and tags it ref.
type Builder interface {
	Build(ctx context.Context, ref string, src Source, progress io.Writer) error
}

// Exister reports whether an image is present in the engine.
type Exister interface {
	ImageExists(ctx context.Context, ref string) (bool, error)
}

// Ensure returns the reference for src, building the image when the engine
// does not have it yet.
func Ensure(ctx context.Context, engine Exister, builder Builder, src Source, progress io.Writer) (string, error) {
	ref, err := src.Ref()
	if err != nil {
		return "", err
	}
	exists, err := engine.ImageExists(ctx, ref)
	if err != nil {
		return "", err
	}
	if exists {
		return ref, nil
	}
	fmt.Fprintf(progress, "building %s\n", ref)
	if err := builder.Build(ctx, ref, src, progress); err != nil {
		return "", err
	}
	return ref, nil
}
