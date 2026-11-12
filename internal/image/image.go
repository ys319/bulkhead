// Package image turns a source directory into a Docker image.
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

// Source is a build context inside a file system: the embedded containers/,
// or an environment's directory with its dev.Dockerfile.
type Source struct {
	Repo string // image repository, e.g. "bulkhead-holder"
	FS   fs.FS
	Dir  string
	// Dockerfile is its name inside Dir; empty means "Dockerfile".
	Dockerfile string
	// Skip leaves files out of the context (and of the hash).
	Skip func(name string) bool
	// Base is what the result depends on besides the files: the image the
	// Dockerfile starts from. It goes into the tag.
	Base string
}

// Options change how an image is built, not what from.
type Options struct {
	Pull    bool // look for newer versions of the images the Dockerfile starts from
	NoCache bool // run every step again
}

// Ref is the image reference for this exact source: the tag is a hash of the
// files, so a newer bh builds a new image instead of silently reusing a
// stale one, and "was this image built from this binary" is visible by name.
func (s Source) Ref() (string, error) {
	sum := sha256.New()
	if s.Base != "" || s.Dockerfile != "" {
		fmt.Fprintf(sum, "%s\x00%s\x00", s.Base, s.Dockerfile)
	}
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
		if s.Skip != nil && s.Skip(path) {
			return nil
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
	Build(ctx context.Context, ref string, src Source, opts Options, progress io.Writer) error
}

// Exister reports whether an image is present in the engine.
type Exister interface {
	ImageExists(ctx context.Context, ref string) (bool, error)
}

// Ensure returns the reference for src, building the image when the engine
// does not have it yet.
func Ensure(ctx context.Context, engine Exister, builder Builder, src Source, progress io.Writer) (string, error) {
	return EnsureWith(ctx, engine, builder, src, nil, progress)
}

// EnsureWith is Ensure, or with rebuild a build in any case: the way to
// pick up what changed outside the source (a newer upstream image, newer
// packages).
func EnsureWith(ctx context.Context, engine Exister, builder Builder, src Source, rebuild *Options, progress io.Writer) (string, error) {
	ref, err := src.Ref()
	if err != nil {
		return "", err
	}
	opts := Options{}
	if rebuild != nil {
		opts = *rebuild
	} else {
		exists, err := engine.ImageExists(ctx, ref)
		if err != nil {
			return "", err
		}
		if exists {
			return ref, nil
		}
	}
	fmt.Fprintf(progress, "building %s\n", ref)
	if err := builder.Build(ctx, ref, src, opts, progress); err != nil {
		return "", err
	}
	return ref, nil
}
