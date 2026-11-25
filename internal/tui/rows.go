package tui

import (
	"fmt"
	"strings"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/doctor"
	"github.com/ys319/bulkhead/internal/ops"
)

// rowKind says what a row of the list is.
type rowKind int

const (
	rowHeading rowKind = iota // a section's title; cannot be selected
	rowBlank                  // cannot be selected
	rowEnv
	rowBroken
	rowKey
	rowCreate
	rowAddRemote
	rowRegisterKey
	rowSettings
	rowMachineKey
)

// row is one line of the list. id identifies it across reloads: the cursor
// is kept by id, so it stays on the same thing when rows come and go.
type row struct {
	kind  rowKind
	id    string
	label string
	style style
	// mark follows the label: ports of the environment are being carried.
	mark string

	env    *EnvState
	broken *config.Broken
	key    *config.KeyEntry
}

func (r row) selectable() bool { return r.kind != rowHeading && r.kind != rowBlank }

// reachState is what is known about reaching a remote environment.
type reachState struct {
	checking bool
	problem  string // empty after a check: it worked
	checked  bool
}

// envStyle is the color of an environment's name in the list: the one place
// its state shows there. Red (broken in a way that needs attention) wins
// over magenta (not isolated — the Docker socket opt-in), which wins over
// yellow, green and gray.
func envStyle(state EnvState, reach reachState) style {
	if !state.Env.Local() {
		switch {
		case reach.checked && reach.problem == "":
			return style{color: green}
		case reach.checked:
			return style{color: red}
		}
		return style{color: gray}
	}
	if state.Env.Archived != "" {
		return style{color: gray, dim: true}
	}
	summary := ops.StateStopped
	if state.Known {
		summary = state.Status.State()
	}
	running := summary == ops.StateRunning || summary == ops.StateStarting
	// What doctor finds counts while the environment runs. Stopped, a
	// container that is merely not running would paint every name red; it
	// shows again when it is started.
	found := doctor.OK
	if running {
		found = doctor.Worst(state.Findings)
	}
	switch {
	case !state.Known:
		return style{color: gray}
	case summary == ops.StateDegraded || found == doctor.Fail:
		return style{color: red}
	case state.Env.DockerSocket:
		return style{color: magenta, dim: !running}
	case summary == ops.StateStarting || found == doctor.Warn:
		return style{color: yellow}
	case summary == ops.StateRunning:
		return style{color: green}
	}
	return style{color: gray}
}

// buildRows lays the list out: Local, Remote, Keys and System, each ending
// with the row that adds to it — so no section is ever empty.
func buildRows(snap Snapshot, reach map[string]reachState) []row {
	rows := []row{{kind: rowHeading, label: "Local"}}
	for i := range snap.Envs {
		state := &snap.Envs[i]
		if state.Env.Local() {
			rows = append(rows, row{kind: rowEnv, id: "env:" + state.Env.ID, label: state.Env.Name, mark: forwardMark(*state), style: envStyle(*state, reachState{}), env: state})
		}
	}
	for i := range snap.Broken {
		broken := &snap.Broken[i]
		rows = append(rows, row{kind: rowBroken, id: "broken:" + broken.ID, label: broken.ID, style: style{color: red}, broken: broken})
	}
	rows = append(rows, row{kind: rowCreate, id: "create", label: "+ Create environment"},
		row{kind: rowBlank}, row{kind: rowHeading, label: "Remote"})
	for i := range snap.Envs {
		state := &snap.Envs[i]
		if !state.Env.Local() {
			rows = append(rows, row{kind: rowEnv, id: "env:" + state.Env.ID, label: state.Env.Name, mark: forwardMark(*state), style: envStyle(*state, reach[state.Env.ID]), env: state})
		}
	}
	rows = append(rows, row{kind: rowAddRemote, id: "add-remote", label: "+ Add remote environment"},
		row{kind: rowBlank}, row{kind: rowHeading, label: "Keys"})
	for i := range snap.Keys {
		key := &snap.Keys[i]
		rows = append(rows, row{kind: rowKey, id: "key:" + key.Fingerprint, label: sanitize(key.DisplayName()), key: key})
	}
	return append(rows, row{kind: rowRegisterKey, id: "register-key", label: "+ Register key"},
		row{kind: rowBlank}, row{kind: rowHeading, label: "System"},
		row{kind: rowSettings, id: "settings", label: "Settings"},
		row{kind: rowMachineKey, id: "machine-key", label: "Machine key"})
}

// forwardMark is what follows an environment's name while ports of it are
// being carried, by this bh or another: forwarding is always in sight.
func forwardMark(state EnvState) string {
	for _, saved := range state.Forwards {
		if saved.State == "open" || saved.State == "elsewhere" {
			return " ⇄"
		}
	}
	return ""
}

// indexOf finds the row with the id; when it is gone, the first row that
// can be selected.
func indexOf(rows []row, id string) int {
	first := -1
	for i, r := range rows {
		if !r.selectable() {
			continue
		}
		if r.id == id {
			return i
		}
		if first == -1 {
			first = i
		}
	}
	return first
}

// step moves from index to the next selectable row in the direction, and
// stays when there is none.
func step(rows []row, index, direction int) int {
	for i := index + direction; i >= 0 && i < len(rows); i += direction {
		if rows[i].selectable() {
			return i
		}
	}
	return index
}

// window picks which rows to show when they do not all fit: a window of a
// fixed size that follows the cursor, so that nothing jumps at the ends.
// more counts the selectable rows hidden above and below.
func window(rows []row, cursor, height int) (from, to, moreAbove, moreBelow int) {
	if len(rows) <= height {
		return 0, len(rows), 0, 0
	}
	size := height - 2 // a line each for what is hidden above and below
	if size < 1 {
		size = 1
	}
	from = cursor - size/2
	if from < 0 {
		from = 0
	}
	if from+size > len(rows) {
		from = len(rows) - size
	}
	to = from + size
	for _, r := range rows[:from] {
		if r.selectable() {
			moreAbove++
		}
	}
	for _, r := range rows[to:] {
		if r.selectable() {
			moreBelow++
		}
	}
	return from, to, moreAbove, moreBelow
}

// listLines renders the list for a pane with the given inner size.
func listLines(rows []row, cursor, inner, height int) []string {
	from, to, above, below := window(rows, cursor, height)
	hidden := func(count int) string {
		if count == 0 {
			return dim("...")
		}
		return dim(fmt.Sprintf("... %d more", count))
	}
	var lines []string
	if from > 0 {
		lines = append(lines, hidden(above))
	}
	for i := from; i < to; i++ {
		r := rows[i]
		switch {
		case r.kind == rowHeading:
			lines = append(lines, bold(r.label))
		case r.kind == rowBlank:
			lines = append(lines, "")
		case i == cursor:
			chosen := r.style
			chosen.bold = true
			lines = append(lines, "❯ "+chosen.render(fit(r.label+r.mark, inner-2)))
		default:
			lines = append(lines, "  "+r.style.render(strings.TrimRight(fit(r.label+r.mark, inner-2), " ")))
		}
	}
	if to < len(rows) {
		lines = append(lines, hidden(below))
	}
	return lines
}
