// Package snapshot reads and writes the file a snapshot of an environment
// is: one gzip-compressed tar with
//
//	snapshot.json          what it is a snapshot of (Manifest), first
//	dev.Dockerfile         the environment's own image, when it has one
//	volumes/<key>/...      the contents of each volume, as tar read them
//	complete.json          how much there was (Summary), last
//
// The file is written under another name and renamed when complete.json is
// in, so a file with the right name is a whole one; complete.json says the
// same for a file that was copied around. Nothing here unpacks anything on
// this machine: the volume parts go from a container into the file and
// from the file into a container as tar streams.
package snapshot

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Format is the version of the layout above.
const Format = 1

// Kinds of snapshot: why it was taken.
const (
	KindManual         = "manual"
	KindBeforeRollback = "before-rollback"
	KindArchive        = "archive"
	KindExport         = "export"
)

// Extension is how snapshot files end.
const Extension = ".tar.gz"

const (
	manifestName   = "snapshot.json"
	dockerfileName = "dev.Dockerfile"
	summaryName    = "complete.json"
	volumesPrefix  = "volumes/"
	// A Dockerfile is a few kilobytes; more is not one.
	maxSmallFile = 1 << 20
)

// Manifest says what a snapshot is of.
type Manifest struct {
	Format      int         `json:"format"`
	Kind        string      `json:"kind"`
	Environment Environment `json:"environment"`
	Created     time.Time   `json:"created"`
	Note        string      `json:"note,omitempty"`
	// BH and Engine are for the reader of a report, not for decisions: the
	// bh that wrote the file and the platform of its engine.
	BH      string   `json:"bh"`
	Engine  string   `json:"engine"`
	Volumes []Volume `json:"volumes"`
}

// Environment is the environment a snapshot was taken of.
type Environment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Volume is one volume in a snapshot.
type Volume struct {
	Key string `json:"key"`
	// LeftOut are the paths, relative to the volume's root, that were not
	// read. Restoring leaves the same paths alone.
	LeftOut []string `json:"leftOut"`
}

// Volume returns the entry for key.
func (m Manifest) Volume(key string) (Volume, bool) {
	for _, volume := range m.Volumes {
		if volume.Key == key {
			return volume, true
		}
	}
	return Volume{}, false
}

// Keys are the keys of the volumes, in the order they are in the file.
func (m Manifest) Keys() []string {
	keys := make([]string, len(m.Volumes))
	for i, volume := range m.Volumes {
		keys[i] = volume.Key
	}
	return keys
}

// Summary is how much a snapshot holds, by volume.
type Summary struct {
	Volumes map[string]Count `json:"volumes"`
}

// Count is the size of one volume's part.
type Count struct {
	Entries int   `json:"entries"`
	Bytes   int64 `json:"bytes"` // of file contents, uncompressed
}

// Bytes is the size of all parts together.
func (s Summary) Bytes() int64 {
	var total int64
	for _, count := range s.Volumes {
		total += count.Bytes
	}
	return total
}

var (
	keyPattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}$`)
	// A left-out path ends up in a shell script that runs as root on the
	// volume; it comes from a file that may come from anywhere.
	leftOutPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)
)

func (m Manifest) check() error {
	if m.Format != Format {
		return fmt.Errorf("format %d: this bh reads format %d", m.Format, Format)
	}
	if len(m.Volumes) == 0 {
		return errors.New("it holds no volume")
	}
	seen := map[string]bool{}
	for _, volume := range m.Volumes {
		if !keyPattern.MatchString(volume.Key) || seen[volume.Key] {
			return fmt.Errorf("volume %q: not a volume's name, or named twice", volume.Key)
		}
		seen[volume.Key] = true
		for _, left := range volume.LeftOut {
			if !leftOutPattern.MatchString(left) || slices.Contains(strings.Split(left, "/"), "..") || slices.Contains(strings.Split(left, "/"), ".") {
				return fmt.Errorf("volume %q: %q is not a path inside the volume", volume.Key, left)
			}
		}
	}
	return nil
}

// Writer writes a snapshot file.
type Writer struct {
	path    string
	file    *os.File
	gz      *gzip.Writer
	tar     *tar.Writer
	created time.Time
	summary Summary
	done    map[string]bool
	want    []string
}

func partial(path string) string { return path + ".partial" }

// Create starts the file. Nothing is at path until Close; Abort removes
// what was written so far.
func Create(path string, manifest Manifest) (*Writer, error) {
	manifest.Format = Format
	if err := manifest.check(); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("%s exists", path)
	}
	// The contents of a home directory: logins, keys, history.
	file, err := os.OpenFile(partial(path), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	// The fastest level: most of the time goes into reading the volume, and
	// what compresses at all compresses nearly as well.
	gz, _ := gzip.NewWriterLevel(file, gzip.BestSpeed)
	w := &Writer{
		path: path, file: file, gz: gz, tar: tar.NewWriter(gz), created: manifest.Created,
		summary: Summary{Volumes: map[string]Count{}}, done: map[string]bool{}, want: manifest.Keys(),
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err == nil {
		err = w.small(manifestName, append(data, '\n'))
	}
	if err != nil {
		w.Abort()
		return nil, err
	}
	return w, nil
}

func (w *Writer) small(name string, data []byte) error {
	header := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: w.created, Typeflag: tar.TypeReg, Format: tar.FormatPAX}
	if err := w.tar.WriteHeader(header); err != nil {
		return err
	}
	_, err := w.tar.Write(data)
	return err
}

// AddDockerfile puts the environment's dev.Dockerfile in. It goes before
// the volumes.
func (w *Writer) AddDockerfile(data []byte) error {
	if len(w.done) > 0 {
		return errors.New("the Dockerfile goes before the volumes")
	}
	if len(data) > maxSmallFile {
		return errors.New("the Dockerfile is larger than 1 MB")
	}
	return w.small(dockerfileName, data)
}

// AddVolume puts a volume in. read writes the volume's contents as a tar
// stream (names relative to the volume's root) and returns when it is done;
// its error is AddVolume's.
func (w *Writer) AddVolume(key string, read func(out io.Writer) error) (Count, error) {
	if !slices.Contains(w.want, key) || w.done[key] {
		return Count{}, fmt.Errorf("volume %q is not in the manifest, or was added before", key)
	}
	w.done[key] = true

	in, out := io.Pipe()
	failed := make(chan error, 1)
	go func() {
		err := read(out)
		out.CloseWithError(err)
		failed <- err
	}()
	count, err := w.copyVolume(key, in)
	if err == nil {
		// tar pads its output after the end mark; whoever writes it must
		// be allowed to finish.
		_, err = io.Copy(io.Discard, in)
	}
	// Whatever stopped the copy also stops the reader; its own error, if it
	// has one, is the cause.
	in.CloseWithError(err)
	if readErr := <-failed; readErr != nil {
		return count, readErr
	}
	if err != nil {
		return count, err
	}
	w.summary.Volumes[key] = count
	return count, nil
}

func (w *Writer) copyVolume(key string, in io.Reader) (Count, error) {
	var count Count
	source := tar.NewReader(in)
	for {
		header, err := source.Next()
		if errors.Is(err, io.EOF) {
			return count, nil
		}
		if err != nil {
			return count, fmt.Errorf("reading the %s volume: %w", key, err)
		}
		name, err := inside(header.Name)
		if err != nil {
			return count, fmt.Errorf("the %s volume: %w", key, err)
		}
		if header.Typeflag == tar.TypeLink {
			// Another name of a file that is already in: the name is one of
			// this archive, and moves with it.
			target, err := inside(header.Linkname)
			if err != nil {
				return count, fmt.Errorf("the %s volume: %w", key, err)
			}
			header.Linkname = volumesPrefix + key + "/" + target
		}
		header.Name = volumesPrefix + key + "/" + name
		if header.Typeflag == tar.TypeDir {
			header.Name += "/"
		}
		// The one format that holds everything the others can.
		header.Format = tar.FormatPAX
		if err := w.tar.WriteHeader(header); err != nil {
			return count, fmt.Errorf("the %s volume: %s: %w", key, name, err)
		}
		if _, err := io.Copy(w.tar, source); err != nil {
			return count, fmt.Errorf("the %s volume: %s: %w", key, name, err)
		}
		count.Entries++
		if header.Typeflag == tar.TypeReg {
			count.Bytes += header.Size
		}
	}
}

// inside puts the name of an entry in one form — no leading "./" or "/",
// "." for the root itself — and refuses one that leaves the volume.
func inside(name string) (string, error) {
	if slices.Contains(strings.Split(name, "/"), "..") {
		return "", fmt.Errorf("the entry %q leads out of the volume", name)
	}
	cleaned := path.Clean("/" + name)[1:]
	if cleaned == "" {
		return ".", nil
	}
	return cleaned, nil
}

// Close ends the file and gives it its name. Every volume of the manifest
// has to be in.
func (w *Writer) Close() (Summary, error) {
	for _, key := range w.want {
		if _, added := w.summary.Volumes[key]; !added {
			w.Abort()
			return Summary{}, fmt.Errorf("volume %q was not added", key)
		}
	}
	data, err := json.MarshalIndent(w.summary, "", "  ")
	if err == nil {
		err = w.small(summaryName, append(data, '\n'))
	}
	for _, step := range []func() error{w.tar.Close, w.gz.Close, w.file.Sync, w.file.Close} {
		if err == nil {
			err = step()
		}
	}
	if err == nil {
		err = os.Rename(partial(w.path), w.path)
	}
	if err != nil {
		w.Abort()
		return Summary{}, err
	}
	return w.summary, nil
}

// Abort removes what was written. After Close it does nothing.
func (w *Writer) Abort() {
	w.file.Close()
	os.Remove(partial(w.path))
}

// reader walks a snapshot file from its start.
type reader struct {
	file     *os.File
	gz       *gzip.Reader
	tar      *tar.Reader
	Manifest Manifest
	// pending is a header that was read and not consumed.
	pending *tar.Header
}

func open(path string) (*reader, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(file)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("%s is not a snapshot: %w", path, err)
	}
	r := &reader{file: file, gz: gz, tar: tar.NewReader(gz)}
	header, err := r.tar.Next()
	if err != nil || header.Name != manifestName || header.Size > maxSmallFile {
		file.Close()
		return nil, fmt.Errorf("%s is not a snapshot: it does not start with %s", path, manifestName)
	}
	decoder := json.NewDecoder(r.tar)
	if err := decoder.Decode(&r.Manifest); err == nil {
		err = r.Manifest.check()
	}
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

func (r *reader) next() (*tar.Header, error) {
	if header := r.pending; header != nil {
		r.pending = nil
		return header, nil
	}
	return r.tar.Next()
}

// ReadManifest reads what the snapshot is of: the start of the file only.
func ReadManifest(path string) (Manifest, error) {
	r, err := open(path)
	if err != nil {
		return Manifest{}, err
	}
	defer r.file.Close()
	return r.Manifest, nil
}

// Contents is what reading a snapshot through found.
type Contents struct {
	Manifest   Manifest
	Summary    Summary
	Dockerfile []byte // nil when it holds none
}

// Verify reads the whole file and checks that it is a complete snapshot:
// the compression's own checksum, the order of its parts, and the counts
// of complete.json against what is there. It is what stands between a
// damaged file and the volumes a rollback would empty for it.
func Verify(path string) (Contents, error) {
	return walk(path, nil, nil)
}

// Restore reads the file and hands each volume in keys (nil: all), in the
// file's order, to write as a tar stream with names relative to the
// volume's root. write returns when it has read the stream to its end. The
// file should have passed Verify: a volume that write has replaced is not
// brought back by an error further on.
func Restore(path string, keys []string, write func(volume Volume, in io.Reader) error) (Contents, error) {
	return walk(path, keys, write)
}

func walk(path string, keys []string, write func(Volume, io.Reader) error) (Contents, error) {
	r, err := open(path)
	if err != nil {
		return Contents{}, err
	}
	defer r.file.Close()
	contents := Contents{Manifest: r.Manifest}
	found := Summary{Volumes: map[string]Count{}}
	fail := func(format string, args ...any) (Contents, error) {
		return contents, fmt.Errorf("%s: %s", path, fmt.Sprintf(format, args...))
	}

	header, err := r.next()
	if err == nil && header.Name == dockerfileName {
		if header.Size > maxSmallFile {
			return fail("the Dockerfile is larger than 1 MB")
		}
		if contents.Dockerfile, err = io.ReadAll(r.tar); err != nil {
			return fail("%v", err)
		}
		header, err = r.next()
	}
	for _, volume := range r.Manifest.Volumes {
		if err != nil {
			return fail("it ends before the %s volume: %v", volume.Key, err)
		}
		r.pending = header
		var count Count
		if write != nil && (keys == nil || slices.Contains(keys, volume.Key)) {
			count, err = r.hand(volume, write)
		} else {
			count, err = r.volume(volume.Key, nil)
		}
		if err != nil {
			return contents, err
		}
		found.Volumes[volume.Key] = count
		header, err = r.next()
	}
	if err != nil || header.Name != summaryName || header.Size > maxSmallFile {
		return fail("it does not end with %s: the file is cut short or not a snapshot", summaryName)
	}
	if err := json.NewDecoder(r.tar).Decode(&contents.Summary); err != nil {
		return fail("%s: %v", summaryName, err)
	}
	if _, err := r.tar.Next(); !errors.Is(err, io.EOF) {
		return fail("there is something after %s (%v)", summaryName, err)
	}
	// To the very end of the compressed stream: tar stops reading at its
	// own end mark, and the checksum of the compression comes after it.
	if _, err := io.Copy(io.Discard, r.gz); err != nil {
		return fail("the end of the file is damaged: %v", err)
	}
	for key, count := range found.Volumes {
		if contents.Summary.Volumes[key] != count {
			return fail("the %s volume has %d entries and %d bytes; %s says %d and %d", key,
				count.Entries, count.Bytes, summaryName, contents.Summary.Volumes[key].Entries, contents.Summary.Volumes[key].Bytes)
		}
	}
	return contents, nil
}

// hand gives the volume's part to write as a tar stream of its own.
func (r *reader) hand(volume Volume, write func(Volume, io.Reader) error) (Count, error) {
	in, out := io.Pipe()
	failed := make(chan error, 1)
	go func() {
		err := write(volume, in)
		// A writer that stops early stops the copy too.
		in.CloseWithError(err)
		failed <- err
	}()
	stream := tar.NewWriter(out)
	count, err := r.volume(volume.Key, stream)
	if err == nil {
		err = stream.Close()
	}
	out.CloseWithError(err)
	if writeErr := <-failed; writeErr != nil {
		return count, fmt.Errorf("writing the %s volume: %w", volume.Key, writeErr)
	}
	return count, err
}

// volume reads the entries of one volume, to out when it is given, and
// stops before the first entry that is not the volume's.
func (r *reader) volume(key string, out *tar.Writer) (Count, error) {
	var count Count
	prefix := volumesPrefix + key + "/"
	for {
		header, err := r.next()
		if errors.Is(err, io.EOF) {
			return count, nil
		}
		if err != nil {
			return count, fmt.Errorf("reading the %s volume: %w", key, err)
		}
		if !strings.HasPrefix(header.Name, prefix) {
			r.pending = header
			return count, nil
		}
		name, err := inside(strings.TrimPrefix(header.Name, prefix))
		if err != nil {
			return count, fmt.Errorf("the %s volume: %w", key, err)
		}
		count.Entries++
		if header.Typeflag == tar.TypeReg {
			count.Bytes += header.Size
		}
		if out == nil {
			// Read, not skipped: a damaged part shows here.
			if _, err := io.Copy(io.Discard, r.tar); err != nil {
				return count, fmt.Errorf("the %s volume: %s: %w", key, name, err)
			}
			continue
		}
		if header.Typeflag == tar.TypeLink {
			if !strings.HasPrefix(header.Linkname, prefix) {
				return count, fmt.Errorf("the %s volume: %s is a link to %q, outside the volume", key, name, header.Linkname)
			}
			target, err := inside(strings.TrimPrefix(header.Linkname, prefix))
			if err != nil {
				return count, fmt.Errorf("the %s volume: %w", key, err)
			}
			header.Linkname = "./" + target
		}
		header.Name = "./" + name
		if name == "." {
			header.Name = "./"
		} else if header.Typeflag == tar.TypeDir {
			header.Name += "/"
		}
		header.Format = tar.FormatPAX
		if err := out.WriteHeader(header); err != nil {
			return count, fmt.Errorf("the %s volume: %s: %w", key, name, err)
		}
		if _, err := io.Copy(out, r.tar); err != nil {
			return count, fmt.Errorf("the %s volume: %s: %w", key, name, err)
		}
	}
}
