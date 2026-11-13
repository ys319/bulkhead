// Package enginetest provides an in-memory stand-in for the Docker Engine,
// for tests of the code that drives it.
package enginetest

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
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

	// Created keeps what each container was created from, by name.
	Created map[string]Spec
	// Output is what Logs and Exec write, by container name.
	Output map[string]string
	// ExecExit is the exit status Exec reports.
	ExecExit int
	// Execs are the commands Exec was asked to run.
	Execs []engine.ExecSpec
	// Devices are the CDI devices the engine reports; nil means it does not
	// report devices at all.
	Devices []string
	// DeviceGroups is the answer to the device probe.
	DeviceGroups []int
	// Sizes are the sizes of the volumes, by name.
	Sizes map[string]int64
	// MissingImage says which images the engine does not have (nil: it has all).
	MissingImage func(ref string) bool
	// Tags are the names images were given.
	Tags map[string]string
	// VolumeRuns are the scripts that were run on volumes, in order.
	VolumeRuns []VolumeRun
	// VolumeJobErr makes running a script on a volume fail.
	VolumeJobErr error
	// Data are the files in the volumes: by volume, then by path relative
	// to its root. ReadVolume reads them and ReplaceVolume replaces them.
	Data map[string]map[string]string
	// ImageFiles are the files the images have, by directory and then by
	// path under it: what RestoreFromImage copies from.
	ImageFiles map[string]map[string]string
	// VolumeTars are the volumes that were read or replaced, in order.
	VolumeTars []engine.VolumeTar
	// VolumeTarErr makes reading and replacing a volume fail.
	VolumeTarErr error
	// EngineInfo is what Info reports (zero: a stock answer); InfoErr makes
	// the engine one that does not answer.
	EngineInfo engine.Info
	InfoErr    error

	nextID      int
	holderPolls int
}

// Spec is what a container was created from.
type Spec struct {
	Config *container.Config
	Host   *container.HostConfig
}

// New returns an engine with no containers whose holder gets healthy at once.
func New() *Fake {
	return &Fake{
		Containers: map[string]*engine.Container{}, Volumes: map[string]bool{}, HealthAfterStart: []string{"healthy"},
		Created: map[string]Spec{}, Output: map[string]string{}, Tags: map[string]string{},
	}
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

// ImageExists says yes unless MissingImage says the image is not there.
func (f *Fake) ImageExists(_ context.Context, ref string) (bool, error) {
	return f.MissingImage == nil || !f.MissingImage(ref), nil
}

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
	f.Created[name] = Spec{Config: config, Host: host}
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
	delete(f.Data, name)
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
		if !strings.HasPrefix(call, "network ") && !strings.HasPrefix(call, "volume ") && !strings.HasPrefix(call, "probe-") && call != "volume-sizes" {
			out = append(out, call)
		}
	}
	return out
}

// Logs writes the container's scripted output.
func (f *Fake) Logs(_ context.Context, id string, opts engine.LogOptions, out io.Writer) error {
	name := f.nameOf(id)
	f.log("logs %s follow=%v tail=%d", name, opts.Follow, opts.Tail)
	_, err := io.WriteString(out, f.Output[name])
	return err
}

// Exec records the command and plays the container's scripted output.
func (f *Fake) Exec(_ context.Context, id string, spec engine.ExecSpec, streams engine.Streams) (int, error) {
	name := f.nameOf(id)
	f.log("exec %s %s", name, strings.Join(spec.Cmd, " "))
	f.Execs = append(f.Execs, spec)
	if streams.Out != nil {
		io.WriteString(streams.Out, f.Output[name])
	}
	return f.ExecExit, nil
}

// TagImage records the new name.
func (f *Fake) TagImage(_ context.Context, source, target string) error {
	f.Tags[target] = source
	return nil
}

// CDIDevices reports Devices.
func (f *Fake) CDIDevices(context.Context) ([]string, bool, error) {
	return f.Devices, f.Devices != nil, nil
}

// DeviceGIDs answers the device probe.
func (f *Fake) DeviceGIDs(_ context.Context, _ string, devices []string) ([]int, error) {
	f.log("probe-devices %s", strings.Join(devices, ","))
	return f.DeviceGroups, nil
}

// Watch reports nothing until the context ends.
func (f *Fake) Watch(ctx context.Context, _ string) (<-chan struct{}, <-chan error) {
	changes, done := make(chan struct{}), make(chan error, 1)
	go func() {
		<-ctx.Done()
		done <- nil
		close(changes)
	}()
	return changes, done
}

// VolumeSizes reports Sizes.
func (f *Fake) VolumeSizes(context.Context) (map[string]int64, error) {
	f.log("volume-sizes")
	return f.Sizes, nil
}

// VolumeRun is one script that was run on a volume.
type VolumeRun struct {
	Job engine.VolumeJob
	// Input is what the script got on its standard input; nil when it was
	// run without one.
	Input []byte
}

// volumeJob records the job. Like the engine's own mount, it would bring a
// missing volume into being without labels, so here that is an error.
func (f *Fake) volumeJob(verb string, job engine.VolumeJob, input []byte) error {
	f.log("%s %s", verb, job.Volume)
	if f.VolumeJobErr != nil {
		return f.VolumeJobErr
	}
	if !f.Volumes[job.Volume] {
		return fmt.Errorf("the volume %s does not exist", job.Volume)
	}
	f.VolumeRuns = append(f.VolumeRuns, VolumeRun{Job: job, Input: input})
	return nil
}

// WriteVolume records the script and its input.
func (f *Fake) WriteVolume(_ context.Context, job engine.VolumeJob, input []byte) error {
	return f.volumeJob("write-volume", job, append([]byte{}, input...))
}

// RunInVolume records the script.
func (f *Fake) RunInVolume(_ context.Context, job engine.VolumeJob) error {
	return f.volumeJob("run-in-volume", job, nil)
}

// Describe answers from what the container was created from (Created) and
// its current state. A container that was put in place without a spec
// (AddLegacy) has only its state.
func (f *Fake) Describe(ctx context.Context, name string) (*engine.Description, error) {
	found, err := f.Container(ctx, name)
	if found == nil || err != nil {
		return nil, err
	}
	described := engine.Description{Name: name}
	if spec, ok := f.Created[name]; ok {
		described = engine.DescribeSpec(name, spec.Config, spec.Host)
	}
	described.Container = *found
	return &described, nil
}

// Info reports EngineInfo, or InfoErr when that is set.
func (f *Fake) Info(context.Context) (engine.Info, error) {
	if f.InfoErr != nil {
		return engine.Info{}, f.InfoErr
	}
	if f.EngineInfo == (engine.Info{}) {
		return engine.Info{Version: "29.0.0", APIVersion: "1.52", OS: "linux", Arch: "amd64", Builder: "2"}, nil
	}
	return f.EngineInfo, nil
}

// leftOut reports whether name is one of the paths, or under one.
func leftOut(name string, paths []string) bool {
	for _, left := range paths {
		if name == left || strings.HasPrefix(name, left+"/") {
			return true
		}
	}
	return false
}

// ReadVolume writes the files of the volume (Data) as a tar stream, as tar
// does in the container: the root first, names in order, left-out paths
// not at all.
func (f *Fake) ReadVolume(_ context.Context, job engine.VolumeTar, out io.Writer) error {
	f.log("read-volume %s", job.Volume)
	if f.VolumeTarErr != nil {
		return f.VolumeTarErr
	}
	if !f.Volumes[job.Volume] {
		return fmt.Errorf("the volume %s does not exist", job.Volume)
	}
	f.VolumeTars = append(f.VolumeTars, job)
	w := tar.NewWriter(out)
	if err := w.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755, Uid: 1000, Gid: 1000}); err != nil {
		return err
	}
	names := make([]string, 0, len(f.Data[job.Volume]))
	for name := range f.Data[job.Volume] {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if leftOut(name, job.LeftOut) {
			continue
		}
		data := f.Data[job.Volume][name]
		if err := w.WriteHeader(&tar.Header{Name: "./" + name, Typeflag: tar.TypeReg, Mode: 0o644, Uid: 1000, Gid: 1000, Size: int64(len(data))}); err != nil {
			return err
		}
		if _, err := w.Write([]byte(data)); err != nil {
			return err
		}
	}
	return w.Close()
}

// ReplaceVolume does to Data what the script does to the volume: every
// file goes except the left-out ones, then the stream's files come in.
func (f *Fake) ReplaceVolume(_ context.Context, job engine.VolumeTar, in io.Reader) error {
	f.log("replace-volume %s", job.Volume)
	if f.VolumeTarErr != nil {
		return f.VolumeTarErr
	}
	if !f.Volumes[job.Volume] {
		return fmt.Errorf("the volume %s does not exist", job.Volume)
	}
	f.VolumeTars = append(f.VolumeTars, job)
	if f.Data == nil {
		f.Data = map[string]map[string]string{}
	}
	kept := map[string]string{}
	for name, data := range f.Data[job.Volume] {
		if leftOut(name, job.LeftOut) {
			kept[name] = data
		}
	}
	f.Data[job.Volume] = kept
	r := tar.NewReader(in)
	for {
		header, err := r.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		kept[strings.TrimPrefix(header.Name, "./")] = string(data)
	}
}

// RestoreFromImage does to Data what the script does to the volume: the
// paths go, and what the image has at them (ImageFiles, by where the
// volume is mounted) comes in.
func (f *Fake) RestoreFromImage(_ context.Context, job engine.VolumeTar, paths []string) error {
	f.log("restore-from-image %s %s", job.Volume, strings.Join(paths, ","))
	if f.VolumeTarErr != nil {
		return f.VolumeTarErr
	}
	if !f.Volumes[job.Volume] {
		return fmt.Errorf("the volume %s does not exist", job.Volume)
	}
	if f.Data == nil {
		f.Data = map[string]map[string]string{}
	}
	if f.Data[job.Volume] == nil {
		f.Data[job.Volume] = map[string]string{}
	}
	for name := range f.Data[job.Volume] {
		if leftOut(name, paths) {
			delete(f.Data[job.Volume], name)
		}
	}
	for name, data := range f.ImageFiles[job.Target] {
		if leftOut(name, paths) {
			f.Data[job.Volume][name] = data
		}
	}
	return nil
}
