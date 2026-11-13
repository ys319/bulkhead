package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ys319/bulkhead/internal/engine"
	"github.com/ys319/bulkhead/internal/ops"
	"github.com/ys319/bulkhead/internal/sshconfig"
)

func (m *Model) View() tea.View {
	view := tea.NewView(strings.Join(m.screen(), "\n"))
	view.AltScreen = true
	return view
}

// screen draws everything, top to bottom: the two panes, the message (only
// when there is one), the line of key hints, and the dialog on top. The
// hints are last so that they stay where they are when a message comes and
// goes; the panes give up the row.
func (m *Model) screen() []string {
	note := m.noteLine()
	bodyRows := m.height - 1
	if note != "" {
		bodyRows--
	}
	if bodyRows < 5 {
		bodyRows = 5
	}
	listInner := listWidth - 4
	detailInner := m.width - listWidth - 4
	if detailInner < 10 {
		detailInner = 10
	}
	cursor := indexOf(m.rows, m.cursor)
	left := box(listLines(m.rows, cursor, listInner, bodyRows-2), listInner, bodyRows)
	right := box(m.detail(detailInner, bodyRows-2), detailInner, bodyRows)
	lines := sideBySide(left, right)

	if note != "" {
		lines = append(lines, note)
	}
	// The last line: what is wrong with live updates on the left, the keys
	// that work now on the right.
	notice := ""
	if m.degraded {
		notice = "live updates degraded — press r to refresh"
	}
	hint := m.hint()
	gap := m.width - width(notice) - width(hint)
	if gap < 1 {
		gap = 1
	}
	lines = append(lines, dim(notice)+strings.Repeat(" ", gap)+dim(hint))

	if d := m.top(); d != nil {
		dialog := m.dialog(d)
		x := (m.width - width(dialog[0])) / 2
		// Centred on the screen, not on the panes: a message that comes or
		// goes must not move a dialog that is open.
		y := (m.height - 1 - len(dialog)) / 2
		if x < 0 {
			x = 0
		}
		if y < 0 {
			y = 0
		}
		lines = overlay(lines, dialog, x, y)
	}
	return lines
}

// hint names the keys that do something now.
func (m *Model) hint() string {
	if d := m.top(); d != nil {
		return d.hint()
	}
	hint := "↑↓ move · enter menu · r reload · q quit"
	if m.note != nil {
		hint = "m message · " + hint
	}
	return hint
}

// noteLine is the message, always one line: the first line, cut to the
// screen, with a count of the lines not shown. The whole text is behind m.
func (m *Model) noteLine() string {
	if m.note == nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(m.note.text), "\n")
	text := sanitize(lines[0])
	suffix := ""
	if len(lines) > 1 {
		suffix = fmt.Sprintf(" (+%d lines)", len(lines)-1)
	}
	room := m.width - width(suffix)
	if width(text) > room {
		text = fit(text, room)
	}
	return paint(m.note.color, text) + dim(suffix)
}

// dialog draws a dialog as wide as its content, within limits: never
// narrower than 40 cells (so that moving between menus does not resize the
// frame) and never wider than the screen allows.
func (m *Model) dialog(d modal) []string {
	widest := m.width - 12
	if widest < 20 {
		widest = 20
	}
	inner := 36
	if _, working := d.(*busy); working {
		inner = 56
	} else if sized, ok := d.(interface{ natural(*Model, int) int }); ok {
		inner = max(inner, sized.natural(m, widest))
	} else {
		for _, line := range d.lines(m, widest) {
			if strings.Trim(sanitize(line), "─ ") == "" {
				continue // a separator is as wide as it is told to be
			}
			if w := width(line); w > inner {
				inner = w
			}
		}
	}
	if w := width(d.title()); w > inner {
		inner = w
	}
	if inner > widest {
		inner = widest
	}
	content := append([]string{bold(d.title()), ""}, d.lines(m, inner)...)
	return box(content, inner, 0)
}

// field is a "label: value" line of the detail pane; a long value wraps
// under itself, to the right of the label.
func field(inner int, label, value string) []string {
	head := label + ": "
	room := inner - width(head)
	if room < 8 {
		return []string{head + value}
	}
	var lines []string
	for i, part := range wrap(value, room) {
		if i == 0 {
			lines = append(lines, dim(head)+part)
		} else {
			lines = append(lines, strings.Repeat(" ", width(head))+part)
		}
	}
	return lines
}

// roughly rounds a duration to its largest unit: 45s, 3m, 5h, 2d.
func roughly(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	}
	return strconv.Itoa(int(d.Hours())/24) + "d"
}

// address is where the environment's ssh server is reached from here.
func address(state EnvState) string {
	host := "127.0.0.1"
	switch {
	case !state.Env.Local():
		host = state.Env.Host
	case state.Env.Publish:
		host = "0.0.0.0"
	}
	return host + ":" + strconv.Itoa(state.Env.SSHPort)
}

// detail is the right pane: the selected row in full.
func (m *Model) detail(inner, rows int) []string {
	if !m.loaded {
		return []string{dim("loading...")}
	}
	selected := m.selected()
	if selected == nil {
		return nil
	}
	switch selected.kind {
	case rowEnv:
		if selected.env.Env.Local() {
			return m.localDetail(*selected.env, inner, rows)
		}
		return m.remoteDetail(*selected.env, inner)
	case rowBroken:
		lines := []string{bold(selected.broken.ID) + " " + paint(red, "(broken)"), ""}
		lines = append(lines, wrap(sanitizeLines(selected.broken.Problem), inner)...)
		return append(lines, "", dim("Fix the file, then press r. Its directory: bh path "+selected.broken.ID))
	case rowKey:
		return m.keyDetail(*selected, inner)
	case rowCreate:
		return wrap("Create a new environment on this machine. It gets its own containers, volumes and ssh port; start it from its menu.", inner)
	case rowAddRemote:
		return wrap("Register an environment that runs on another machine, by its address ("+"host:port/name"+"). Its menu then opens a shell or VS Code there.", inner)
	case rowRegisterKey:
		return wrap("Register a public key of another machine, so that it can be allowed into environments here.", inner)
	case rowSettings:
		return wrap("Settings shared by all environments: init.sh, the script that runs once in every new environment.", inner)
	case rowMachineKey:
		return wrap("Show this machine's public key (and copy it), to register it on another machine with bh key add.", inner)
	}
	return nil
}

func (m *Model) localDetail(state EnvState, inner, rows int) []string {
	env := state.Env
	lines := []string{bold(env.Name)}

	// What it is doing now comes first, without a heading: when the pane
	// is too short it is cut from the bottom, and these are the lines that
	// must stay.
	switch {
	case !state.Known:
		lines = append(lines, field(inner, "state", "unknown — "+sanitize(m.snap.EngineProblem))...)
	default:
		summary := string(state.Status.State())
		if devRunning(state) && !state.Status.Dev.StartedAt.IsZero() {
			summary += dim(", up " + roughly(time.Since(state.Status.Dev.StartedAt)))
		}
		lines = append(lines, field(inner, "state", summary)...)
		if state.Status.State() == ops.StateDegraded {
			for _, line := range wrap("holder "+describe(state.Status.Holder)+", dev "+describe(state.Status.Dev)+" — Start repairs it", inner-2) {
				lines = append(lines, "  "+paint(red, line))
			}
		}
	}
	if env.DockerSocket {
		lines = append(lines, field(inner, "docker", paint(magenta, "host Docker socket mounted — not isolated"))...)
	}
	lines = append(lines, doctorLines(state.Findings, inner)...)
	// The disk line is there in every state of the reading, so that nothing
	// below it moves when the sizes arrive. It is absent only for an
	// environment without volumes.
	switch summary, has := DiskSummary(env.ID, m.disk); {
	case has:
		lines = append(lines, field(inner, "disk", summary)...)
	case m.disk == nil && m.diskFailed:
		lines = append(lines, field(inner, "disk", dim("unavailable"))...)
	case m.disk == nil:
		lines = append(lines, field(inner, "disk", dim("fetching..."))...)
	}

	lines = append(lines, bold("Runtime"))
	lines = append(lines, field(inner, "  restart", env.Restart)...)
	switch env.GPU {
	case "legacy":
		lines = append(lines, field(inner, "  gpu", "all (legacy)")...)
	case "cdi":
		lines = append(lines, field(inner, "  gpu", strings.Join(env.CDIDevices, ", "))...)
	}

	lines = append(lines, bold("SSH"))
	where := address(state)
	if env.Publish {
		where += dim(" (LAN)")
	}
	lines = append(lines, field(inner, "  address", where)...)
	lines = append(lines, field(inner, "  alias", sshconfig.Alias(env.Name))...)
	keys := "this machine"
	if extra := len(env.AuthorizedKeys); extra > 0 {
		keys += " + " + strconv.Itoa(extra)
	}
	lines = append(lines, field(inner, "  keys", keys)...)
	lines = append(lines, tokenLines(state, inner)...)

	// The end of the environment's output, as far as there is room.
	if m.logs != nil && m.logs.id == env.ID {
		if room := rows - len(lines) - 1; room >= 3 {
			if tail := m.logs.tail(room); len(tail) > 0 {
				lines = append(lines, bold("Logs"))
				for _, line := range tail {
					lines = append(lines, dim(fit(line, inner)))
				}
			}
		}
	}
	return lines
}

// describe is one container in a few words.
func describe(found *engine.Container) string {
	if found == nil {
		return "not created"
	}
	if found.Running() && found.Health != "" {
		return found.Status + " (" + found.Health + ")"
	}
	return found.Status
}

func (m *Model) remoteDetail(state EnvState, inner int) []string {
	lines := []string{bold(state.Env.Name) + dim(" (remote)")}
	reach := m.reach[state.Env.ID]
	switch {
	case reach.checking || !reach.checked:
		lines = append(lines, field(inner, "reach", dim("checking..."))...)
	case reach.problem == "":
		lines = append(lines, field(inner, "reach", paint(green, "ok"))...)
	default:
		lines = append(lines, field(inner, "reach", paint(red, "failed")+" — "+sanitize(firstLine(reach.problem)))...)
	}
	lines = append(lines, bold("SSH"))
	lines = append(lines, field(inner, "  address", address(state))...)
	return append(lines, field(inner, "  alias", sshconfig.Alias(state.Env.Name))...)
}

func (m *Model) keyDetail(selected row, inner int) []string {
	key := selected.key
	lines := []string{bold(sanitize(key.DisplayName()))}
	if fields := strings.Fields(key.PublicKey); len(fields) > 0 {
		lines = append(lines, field(inner, "type", sanitize(fields[0]))...)
	}
	lines = append(lines, field(inner, "fingerprint", key.Fingerprint)...)
	users := m.usersOf(key.Fingerprint)
	where := "(none)"
	if len(users) > 0 {
		where = strings.Join(users, ", ")
	}
	return append(lines, field(inner, "environments", where)...)
}

// usersOf are the names of the local environments that let the key in.
func (m *Model) usersOf(fingerprint string) []string {
	var names []string
	for _, state := range m.snap.Envs {
		if !state.Env.Local() {
			continue
		}
		for _, attached := range state.Env.AuthorizedKeys {
			if attached == fingerprint {
				names = append(names, state.Env.Name)
				break
			}
		}
	}
	return names
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}
