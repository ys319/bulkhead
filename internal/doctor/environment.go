package doctor

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"

	"github.com/ys319/bulkhead/internal/engine"
	"github.com/ys319/bulkhead/internal/ops"
	"github.com/ys319/bulkhead/internal/plan"
)

// Environment is what the checks of a local environment look at.
type Environment struct {
	Name string

	// Plan and Target are what bh would create the containers from now.
	// What only a probe on the engine's side can tell (the groups that own
	// the Docker socket and the GPU device nodes) may be left zero: it is
	// read off the dev container, since a probe runs a container and doctor
	// only reads.
	Plan   plan.Env
	Target plan.Target
	// PlanUnknown says why Plan is not all of what bh would use (a key that
	// does not resolve, no machine key yet). The boundary is still checked;
	// whether a container matches the current settings is then not told.
	PlanUnknown string

	// KeysErr is why the environment's key references do not resolve.
	KeysErr error
	// EngineDown: the engine does not answer at all (the machine's checks
	// say so), so the containers were not looked at.
	EngineDown bool
	// InspectErr is why an engine that does answer could not show them.
	InspectErr error

	// Holder and Dev are the containers as the engine has them; nil when
	// they do not exist.
	Holder, Dev *engine.Description
}

// CheckLocal checks a local environment: the settings that give up part of
// the boundary on purpose, its key references, and its containers.
func CheckLocal(in Environment) []Finding {
	var out []Finding
	if in.Plan.DockerSocket {
		out = append(out, standing("dev gets the Docker socket by opt-in (dockerSocket: true) — this environment is not isolated from the host"))
	}
	if in.Plan.Publish {
		out = append(out, standing(fmt.Sprintf("ssh is published beyond this machine (publish: true, port %d) — keys only, but make sure the network is trusted", in.Plan.SSHPort)))
	}
	if in.KeysErr != nil {
		out = append(out, fail(in.KeysErr.Error()))
	}
	switch {
	case in.EngineDown:
		// Counted once, as the machine's failure.
		return append(out, skip("containers not checked: the Docker Engine does not answer"))
	case in.InspectErr != nil:
		return append(out, fail(fmt.Sprintf("the containers could not be inspected: %v", in.InspectErr)))
	}
	return append(out, checkContainers(in)...)
}

// CheckRemote is the section of a remote environment: its containers are
// on another machine's Docker.
func CheckRemote() []Finding {
	return []Finding{skip("remote environment — run bh doctor on the machine it lives on")}
}

// CheckBroken is the section of an environment whose settings cannot be
// read. It counts as a failure: a diagnosis that passes over it would say
// "no failures" about an environment nobody can start.
func CheckBroken(problem string) []Finding {
	return []Finding{fail(problem + "\n(fix the file and retry)")}
}

func checkContainers(in Environment) []Finding {
	holder, dev := in.Holder, in.Dev
	if state(holder, dev) == ops.StateAbsent {
		return []Finding{skip(fmt.Sprintf("not created yet (bh up %s creates and starts it)", in.Name))}
	}

	env := withGroupsOf(in.Plan, dev)
	wantHolder := planned(plan.Holder(env, in.Target))
	holderID := ""
	if holder != nil {
		holderID = holder.ID
	}
	wantDev := planned(plan.Dev(env, in.Target, holderID))

	out := liveness(in.Name, holder, dev)
	if in.PlanUnknown != "" {
		out = append(out, skip("not compared with the current settings: "+in.PlanUnknown))
	}

	// A stopped container is checked only if the next bh up would start it
	// as it is. One that is not what bh would create now is replaced then
	// (ops.Up: another spec label, or a dev that refers to another holder),
	// and comparing it now would turn "stopped it, changed a setting" into
	// a list of failures.
	current := func(got *engine.Description, want engine.Description) bool {
		return got != nil && in.PlanUnknown == "" && got.Labels[plan.LabelSpec] == want.Labels[plan.LabelSpec]
	}
	keepHolder := current(holder, wantHolder)
	keepDev := keepHolder && current(dev, wantDev) && dev.NetworkMode == plan.JoinMode(holderID)
	replaced := false
	if holder != nil {
		if active(holder) || keepHolder {
			out = append(out, checkHolder(in, *holder, wantHolder)...)
		} else {
			replaced = true
		}
	}
	if dev != nil {
		if active(dev) || keepDev {
			out = append(out, checkDev(in, *dev, wantDev)...)
		} else {
			replaced = true
		}
	}
	if replaced && in.PlanUnknown == "" {
		out = append(out, skip(fmt.Sprintf("the stopped containers are not checked: they are not what bh would create now (other settings, or another bh), and bh up %s replaces them", in.Name)))
	}
	return out
}

// withGroupsOf fills in the groups a probe would report from the groups
// dev was created with, so that the planned spec can be compared with it.
// A dev created for other groups than the settings ask for still comes out
// different.
func withGroupsOf(env plan.Env, dev *engine.Description) plan.Env {
	if dev == nil || len(dev.GroupAdd) == 0 {
		return env
	}
	gids := make([]int, 0, len(dev.GroupAdd))
	for _, group := range dev.GroupAdd {
		gid, err := strconv.Atoi(group)
		if err != nil {
			return env
		}
		gids = append(gids, gid)
	}
	if env.DockerSocket {
		env.SocketGID = gids[0]
	}
	if env.GPU == "cdi" {
		env.DeviceGIDs = gids
	}
	return env
}

func planned(spec plan.Container) engine.Description {
	return engine.DescribeSpec(spec.Name, spec.Config, spec.Host)
}

func state(holder, dev *engine.Description) ops.State {
	var status ops.Status
	if holder != nil {
		status.Holder = &holder.Container
	}
	if dev != nil {
		status.Dev = &dev.Container
	}
	return status.State()
}

func restarting(c *engine.Description) bool {
	return c != nil && c.Status == string(container.StateRestarting)
}

// active: the container's process runs, or the engine keeps bringing it back.
func active(c *engine.Description) bool { return c.Running() || restarting(c) }

// liveness says whether the two containers add up to a working environment
// behind a verified filter. It fails exactly when ops calls the state
// degraded, and names the cause.
func liveness(name string, holder, dev *engine.Description) []Finding {
	current := state(holder, dev)
	holderUp := holder != nil && holder.Running()
	devUp := dev != nil && dev.Running()
	// dev is only expected once the holder has proved its filter.
	gateOpen := holderUp && holder.Health == string(container.Healthy)

	var out []Finding
	if current == ops.StateStopped {
		out = append(out, skip("not running"))
	}

	switch {
	case holder == nil:
		if devUp {
			out = append(out, fail(fmt.Sprintf("the holder container is missing while dev runs — dev has no network and no filter in front of it; bh up %s repairs it", name)))
		}
	case restarting(holder):
		out = append(out, fail(fmt.Sprintf("holder keeps exiting (its restart policy brings it back each time) — bh logs %s shows why", name)))
	case !holderUp:
		if devUp {
			out = append(out, fail(fmt.Sprintf("holder is not running (%s) while dev is — dev has no network; bh up %s repairs it", holder.Status, name)))
		}
	default:
		// The healthcheck passes only after the holder installed the filter
		// and proved it bites: this is the gate dev is started behind.
		switch holder.Health {
		case string(container.Healthy):
			out = append(out, ok("holder is healthy: the egress filter passed its self-check"))
		case string(container.Starting):
			out = append(out, warn("holder's self-check is still running — retry shortly"))
		case string(container.Unhealthy):
			out = append(out, fail(fmt.Sprintf("holder is unhealthy: the egress filter did not pass its self-check — bh up %s starts over", name)))
		default:
			out = append(out, fail("holder reports no health: nothing vouches for the egress filter"))
		}
	}

	switch {
	case dev == nil:
		if gateOpen {
			out = append(out, fail(fmt.Sprintf("the dev container is missing (bh up %s creates it)", name)))
		}
	case restarting(dev):
		out = append(out, fail(fmt.Sprintf("dev keeps exiting (its restart policy brings it back each time) — bh logs %s shows why", name)))
	case !devUp:
		if gateOpen {
			out = append(out, fail(fmt.Sprintf("dev is not running while the holder is (bh up %s starts it)", name)))
		}
	case holder != nil && dev.NetworkMode != plan.JoinMode(holder.ID):
		// Anything else than the holder's namespace has no egress filter:
		// the default bridge, the host's network, a holder that is gone.
		mode := dev.NetworkMode
		if mode == "" {
			mode = "default"
		}
		out = append(out, fail(fmt.Sprintf("dev is not in the holder's network namespace (network mode: %s) — it is not behind the egress filter; bh up %s recreates it", mode, name)))
	case holderUp && dev.StartedAt.Before(holder.StartedAt):
		out = append(out, fail(fmt.Sprintf("dev has been running since before the holder last started: it is left in the namespace of the holder's previous run, without network — bh up %s restarts it", name)))
	case gateOpen:
		out = append(out, ok("dev is in the holder's network namespace"))
	}

	// ops is the definition of "degraded"; should it ever know a cause this
	// function does not name, the state must still not pass as healthy.
	if current == ops.StateDegraded && !slices.ContainsFunc(out, func(f Finding) bool { return f.Level == Fail || f.Level == Warn }) {
		out = append(out, fail(fmt.Sprintf("the two containers do not add up to a working environment; bh up %s repairs it", name)))
	}
	return out
}

func checkHolder(in Environment, got, want engine.Description) []Finding {
	const role = "holder"
	out := checkPrivileges(role, got, want)
	// The healthcheck is the gate; any other test passing says nothing
	// about the filter.
	if !slices.Equal(got.Healthcheck, want.Healthcheck) {
		test := "none"
		if len(got.Healthcheck) > 0 {
			test = strings.Join(got.Healthcheck, " ")
		}
		out = append(out, fail(fmt.Sprintf("holder's healthcheck is not the one bh sets (%s) — it does not vouch for the egress filter", test)))
	}
	// On another network (the host's, say) the filter is installed
	// somewhere else than around dev.
	if got.NetworkMode != want.NetworkMode {
		out = append(out, fail(fmt.Sprintf("holder is on the network %q, not on the environment's own (%s)", got.NetworkMode, want.NetworkMode)))
	}
	out = append(out, checkSysctls(role, got, want)...)
	return append(out, checkCommon(in, role, got, want)...)
}

func checkDev(in Environment, got, want engine.Description) []Finding {
	const role = "dev"
	return append(checkPrivileges(role, got, want), checkCommon(in, role, got, want)...)
}

// checkCommon are the comparisons with the plan that read the same for
// both containers.
func checkCommon(in Environment, role string, got, want engine.Description) []Finding {
	out := checkIsolation(role, got)
	out = append(out, checkPorts(role, got, want)...)
	out = append(out, checkMounts(in.Name, role, got, want)...)
	out = append(out, checkGPU(in.Name, role, got, want)...)
	out = append(out, checkRestart(role, got, want)...)
	return append(out, checkAge(in, role, got, want)...)
}

// checkPrivileges: the container can do no more than the plan allows — dev
// nothing at all, the holder only NET_ADMIN (for its filter).
func checkPrivileges(role string, got, want engine.Description) []Finding {
	// Privileged overrides the capability settings: CapDrop still reads
	// "ALL" in the inspection and means nothing. Looked at first, so that
	// no "ok" about capabilities stands next to it.
	if got.Privileged {
		return []Finding{fail(role + " is privileged: it has every capability and every device of the host — cap_drop and no-new-privileges do not apply")}
	}

	var out []Finding
	gotCaps, wantCaps := capabilities(got.CapAdd), capabilities(want.CapAdd)
	extra, missing := difference(gotCaps, wantCaps), difference(wantCaps, gotCaps)
	switch {
	case !slices.Contains(capabilities(got.CapDrop), "ALL"):
		out = append(out, fail(role+" does not drop all capabilities (cap_drop ALL) — it keeps the engine's default set"))
	case len(extra) > 0:
		out = append(out, fail(fmt.Sprintf("%s has capabilities bh does not give it: %s", role, strings.Join(extra, ", "))))
	case len(missing) > 0:
		out = append(out, fail(fmt.Sprintf("%s lacks %s, which bh gives it", role, strings.Join(missing, ", "))))
	case len(wantCaps) == 0:
		out = append(out, ok(role+" has no capabilities (cap_drop ALL)"))
	default:
		out = append(out, ok(fmt.Sprintf("%s has no capability but %s", role, strings.Join(wantCaps, ", "))))
	}

	gotOpts, wantOpts := securityOptions(got.SecurityOpt), securityOptions(want.SecurityOpt)
	for _, option := range difference(wantOpts, gotOpts) {
		if option == noNewPrivileges {
			// Without it a setuid binary gets its capabilities back.
			out = append(out, fail(role+" lacks no-new-privileges — a setuid program in it can gain privileges again"))
		} else {
			out = append(out, fail(fmt.Sprintf("%s lacks the security option %s, which bh sets", role, option)))
		}
	}
	var off []string
	for _, option := range gotOpts {
		key, value, _ := strings.Cut(option, "=")
		if value == "unconfined" || (key == "label" && value == "disable") {
			off = append(off, option)
		}
	}
	if len(off) > 0 {
		out = append(out, fail(fmt.Sprintf("%s has a confinement of the engine turned off: %s", role, strings.Join(off, ", "))))
	}
	if slices.Contains(wantOpts, noNewPrivileges) && slices.Contains(gotOpts, noNewPrivileges) {
		out = append(out, ok(role+" cannot gain privileges (no-new-privileges)"))
	}
	return out
}

// capabilities puts capability names in one form: the engine takes
// "NET_ADMIN", "CAP_NET_ADMIN" and lower case alike, and reports what it
// was given.
func capabilities(list []string) []string {
	out := make([]string, 0, len(list))
	for _, name := range list {
		out = append(out, strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(name)), "CAP_"))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

const noNewPrivileges = "no-new-privileges=true"

// securityOptions puts security options in the form key=value. The engine
// takes ":" and "=" as the separator, and a bare "no-new-privileges".
func securityOptions(list []string) []string {
	out := make([]string, 0, len(list))
	for _, option := range list {
		key, value := option, ""
		if at := strings.IndexAny(option, ":="); at >= 0 {
			key, value = option[:at], option[at+1:]
		}
		if key == "no-new-privileges" && value == "" {
			value = "true"
		}
		out = append(out, key+"="+value)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// difference is what a has and b does not; both sorted.
func difference(a, b []string) []string {
	var out []string
	for _, item := range a {
		if !slices.Contains(b, item) {
			out = append(out, item)
		}
	}
	return out
}

// checkIsolation: the container shares no namespace with the host and has
// none of its device nodes. bh never asks for either; with the process
// namespace of the host a container sees every process on the machine.
func checkIsolation(role string, got engine.Description) []Finding {
	var out []Finding
	if got.PidMode != "" {
		out = append(out, fail(fmt.Sprintf("%s shares a process namespace that is not its own (pid: %s)", role, got.PidMode)))
	}
	if got.IpcMode == "host" || strings.HasPrefix(got.IpcMode, "container:") {
		out = append(out, fail(fmt.Sprintf("%s shares an IPC namespace that is not its own (ipc: %s)", role, got.IpcMode)))
	}
	if got.UsernsMode == "host" {
		out = append(out, fail(role+" is exempt from the engine's user namespace remapping (userns: host)"))
	}
	if len(got.Devices) > 0 {
		out = append(out, fail(fmt.Sprintf("%s has device nodes of the host: %s", role, strings.Join(got.Devices, ", "))))
	}
	return out
}

// checkSysctls: what the plan sets is set. For the holder that is "IPv6
// off" for the shared namespace — the filter is IPv4, and IPv6 would go
// around it.
func checkSysctls(role string, got, want engine.Description) []Finding {
	var out []Finding
	keys := make([]string, 0, len(want.Sysctls))
	for key := range want.Sysctls {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		value, set := got.Sysctls[key]
		switch {
		case !set:
			out = append(out, fail(fmt.Sprintf("%s does not set the sysctl %s (bh sets it to %s)", role, key, want.Sysctls[key])))
		case value != want.Sysctls[key]:
			out = append(out, fail(fmt.Sprintf("%s sets the sysctl %s to %s; bh sets it to %s", role, key, value, want.Sysctls[key])))
		}
	}
	return out
}

func address(binding engine.PortBinding) string {
	ip := binding.HostIP
	switch {
	case ip == "":
		ip = "0.0.0.0" // unspecified is every interface
	case strings.Contains(ip, ":"):
		ip = "[" + ip + "]"
	}
	return ip + ":" + binding.HostPort
}

// checkPorts: only the holder publishes, and only what the settings say.
// The holder owns the namespace; a port published by anything else is not
// part of what the settings describe.
func checkPorts(role string, got, want engine.Description) []Finding {
	var out []Finding
	if got.PublishAllPorts {
		out = append(out, fail(role+" publishes every exposed port on all interfaces (publish-all)"))
	}

	byPort := func(bindings []engine.PortBinding) map[string][]string {
		grouped := map[string][]string{}
		for _, binding := range bindings {
			grouped[binding.ContainerPort] = append(grouped[binding.ContainerPort], address(binding))
		}
		for _, addresses := range grouped {
			slices.Sort(addresses)
		}
		return grouped
	}
	gotPorts, wantPorts := byPort(got.Ports), byPort(want.Ports)

	var extra []string
	for _, binding := range got.Ports {
		if _, wanted := wantPorts[binding.ContainerPort]; !wanted {
			extra = append(extra, binding.ContainerPort+" on "+address(binding))
		}
	}
	switch {
	case len(extra) > 0 && len(wantPorts) == 0:
		out = append(out, fail(fmt.Sprintf("%s publishes ports itself: %s — only the holder may", role, strings.Join(extra, ", "))))
	case len(extra) > 0:
		out = append(out, warn(fmt.Sprintf("%s publishes ports the settings do not ask for: %s", role, strings.Join(extra, ", "))))
	case len(wantPorts) == 0 && !got.PublishAllPorts:
		out = append(out, ok(role+" publishes no ports"))
	}

	ports := make([]string, 0, len(wantPorts))
	for port := range wantPorts {
		ports = append(ports, port)
	}
	slices.Sort(ports)
	for _, port := range ports {
		what := port
		if port == "22/tcp" {
			what = "ssh"
		}
		wanted := strings.Join(wantPorts[port], ", ")
		switch {
		case slices.Equal(gotPorts[port], wantPorts[port]):
			out = append(out, ok(fmt.Sprintf("%s is published on %s, as the settings say", what, wanted)))
		case len(gotPorts[port]) == 0:
			out = append(out, fail(fmt.Sprintf("%s is not published; the settings say %s", what, wanted)))
		default:
			out = append(out, fail(fmt.Sprintf("%s is published on %s; the settings say %s", what, strings.Join(gotPorts[port], ", "), wanted)))
		}
	}
	return out
}

// hostPath puts a path of the host in one form. Docker Desktop reports the
// source of a bind under /host_mnt (macOS) or /run/desktop/mnt/host
// (Windows with WSL 2), and a Windows drive as /c.
func hostPath(path string) string {
	cleaned := strings.ReplaceAll(path, `\`, "/")
	for _, prefix := range []string{"/host_mnt", "/run/desktop/mnt/host"} {
		if rest, found := strings.CutPrefix(cleaned, prefix); found && (rest == "" || rest[0] == '/') {
			cleaned = rest
			break
		}
	}
	letter := func(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }
	if len(cleaned) >= 2 && letter(cleaned[0]) && cleaned[1] == ':' {
		cleaned = "/" + strings.ToLower(cleaned[:1]) + cleaned[2:]
	} else if len(cleaned) >= 2 && cleaned[0] == '/' && cleaned[1] >= 'A' && cleaned[1] <= 'Z' && (len(cleaned) == 2 || cleaned[2] == '/') {
		cleaned = "/" + strings.ToLower(cleaned[1:2]) + cleaned[2:]
	}
	cleaned = strings.TrimRight(cleaned, "/")
	if cleaned == "" && path != "" {
		return "/" // the prefix alone is the root of the host
	}
	return cleaned
}

func isDockerSocket(m engine.Mount) bool {
	return m.Target == plan.DockerSocketPath || strings.HasSuffix(m.Source, "docker.sock")
}

// sameMount: got is the planned mount.
func sameMount(got, want engine.Mount) bool {
	if got.Type != want.Type || got.Target != want.Target {
		return false
	}
	if got.Type != "bind" {
		return got.Source == want.Source
	}
	// Where the engine's socket is on its own side is the engine's
	// business (Docker Desktop reports a path inside its VM).
	return want.Target == plan.DockerSocketPath || hostPath(got.Source) == hostPath(want.Source)
}

// checkMounts: the container sees nothing but what the plan mounts — for
// dev its own volumes and the read-only init.sh, for the holder nothing.
// A path of the host is the boundary given away (the home directory and
// the Docker socket are the worst cases, not the only ones); a volume from
// elsewhere is not the host, so it is a warning.
func checkMounts(name, role string, got, want engine.Description) []Finding {
	var out []Finding
	found := map[string]bool{}
	for _, m := range got.Mounts {
		if m.Type == "tmpfs" {
			continue // memory of its own, nothing of the host
		}
		at := slices.IndexFunc(want.Mounts, func(w engine.Mount) bool { return sameMount(m, w) })
		switch {
		case at >= 0:
			found[m.Target] = true
			if want.Mounts[at].ReadOnly && !m.ReadOnly {
				out = append(out, fail(fmt.Sprintf("%s can write to %s, which bh mounts read-only", role, m.Target)))
			}
		case isDockerSocket(m):
			// The opt-in is for dev; nothing puts the socket in the holder.
			out = append(out, fail(fmt.Sprintf("the Docker socket is mounted into %s, which the settings do not allow — whoever is in the container controls the host's Docker", role)))
		case m.Type == "bind" || m.Type == "npipe" || m.Type == "":
			out = append(out, fail(fmt.Sprintf("%s mounts %s of the host at %s — bh gives a container no path of the host", role, m.Source, m.Target)))
		default:
			out = append(out, warn(fmt.Sprintf("%s mounts the %s %s at %s, which is not part of the environment", role, m.Type, m.Source, m.Target)))
		}
	}
	for _, w := range want.Mounts {
		if !found[w.Target] {
			out = append(out, warn(fmt.Sprintf("%s lacks its mount at %s — bh restart %s recreates it", role, w.Target, name)))
		}
	}
	if len(out) > 0 {
		return out
	}
	switch {
	case len(want.Mounts) == 0:
		return []Finding{ok(role + " mounts nothing")}
	case slices.ContainsFunc(want.Mounts, isDockerSocket):
		return []Finding{ok(role + " mounts the environment's volumes, init.sh and (by opt-in) the Docker socket")}
	}
	return []Finding{ok(role + " mounts only the environment's volumes and init.sh")}
}

func hasGPU(request engine.DeviceRequest) bool {
	return slices.ContainsFunc(request.Capabilities, func(set []string) bool { return slices.Contains(set, "gpu") })
}

func sameRequest(got, want engine.DeviceRequest) bool {
	if got.Driver == "cdi" || want.Driver == "cdi" {
		// The chosen devices as a set: their order means nothing.
		a, b := slices.Sorted(slices.Values(got.DeviceIDs)), slices.Sorted(slices.Values(want.DeviceIDs))
		return got.Driver == want.Driver && slices.Equal(slices.Compact(a), slices.Compact(b))
	}
	return hasGPU(got) == hasGPU(want)
}

func requestText(request engine.DeviceRequest) string {
	switch {
	case request.Driver == "cdi":
		return "cdi: " + strings.Join(request.DeviceIDs, ", ")
	case hasGPU(request):
		return "gpu"
	case request.Driver != "":
		return request.Driver
	}
	return "a device request"
}

// checkGPU: the device requests are the ones the settings ask for (gpu).
// A request adds no capability, so one too many is a warning; one the
// settings ask for and the container lacks means the setting is not in
// effect.
func checkGPU(name, role string, got, want engine.Description) []Finding {
	var out []Finding
	matched := true
	for _, w := range want.DeviceRequests {
		if slices.ContainsFunc(got.DeviceRequests, func(g engine.DeviceRequest) bool { return sameRequest(g, w) }) {
			out = append(out, ok(fmt.Sprintf("%s has the GPU request of the settings (%s)", role, requestText(w))))
			continue
		}
		matched = false
		out = append(out, fail(fmt.Sprintf("%s does not have the GPU request of the settings (%s) — bh restart %s applies it", role, requestText(w), name)))
	}
	if !matched {
		return out
	}
	for _, g := range got.DeviceRequests {
		if !slices.ContainsFunc(want.DeviceRequests, func(w engine.DeviceRequest) bool { return sameRequest(g, w) }) {
			out = append(out, warn(fmt.Sprintf("%s has a device request the settings do not ask for (%s)", role, requestText(g))))
		}
	}
	return out
}

func restartText(policy string) string {
	if policy == "" {
		return "no"
	}
	return policy
}

// checkRestart: both containers follow the policy of the settings. With
// different policies the holder can come back alone, in a fresh namespace,
// and leave dev without network.
func checkRestart(role string, got, want engine.Description) []Finding {
	if restartText(got.RestartPolicy) == restartText(want.RestartPolicy) {
		return nil
	}
	return []Finding{warn(fmt.Sprintf("%s has the restart policy %q; the settings say %q", role, restartText(got.RestartPolicy), restartText(want.RestartPolicy)))}
}

// checkAge: the container is what this bh would create from the current
// settings. The image's tag is a hash of the sources embedded in bh, so a
// different image means another bh built it (or dev.Dockerfile changed).
func checkAge(in Environment, role string, got, want engine.Description) []Finding {
	if in.PlanUnknown != "" {
		return nil
	}
	switch {
	case want.Image != "" && got.Image != want.Image:
		return []Finding{warn(fmt.Sprintf("%s runs the image %s; this bh would use %s (it was created by an older bh, or dev.Dockerfile changed) — bh restart %s recreates it", role, got.Image, want.Image, in.Name))}
	case got.Labels[plan.LabelSpec] != want.Labels[plan.LabelSpec]:
		return []Finding{warn(fmt.Sprintf("%s was created from other settings than the current ones — the next bh up %s recreates it", role, in.Name))}
	}
	return nil
}
