package tui

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ys319/bulkhead/internal/ops"
)

// listWidth is the outer width of the list pane: border and padding (4),
// the cursor (2) and the longest name an environment can have (32).
const listWidth = 38

// note is the line under the status line: the outcome of the last action.
type note struct {
	color color
	text  string
}

// Model is the state of the interface.
type Model struct {
	backend Backend
	ctx     context.Context

	width, height int
	frame         int

	snap       Snapshot
	loaded     bool
	generation int // of the newest reload asked for; older answers are dropped
	// own is the reload that follows an action taken here: what it changed
	// is already said by the action's own message.
	own    int
	rows   []row
	cursor string // the id of the selected row
	reach  map[string]reachState

	modals []modal
	note   *note
	// degraded: the connection that reports container changes is down, so
	// the list may be stale until r is pressed.
	degraded bool
	pending  bool // a reload is already scheduled after a change

	logs *logTail
}

// New returns the interface in its initial state.
func New(ctx context.Context, backend Backend) *Model {
	return &Model{backend: backend, ctx: ctx, width: 80, height: 24, reach: map[string]reachState{}}
}

// Messages.
type (
	loadedMsg struct {
		generation int
		snap       Snapshot
		err        error
		probe      bool
	}
	changedMsg struct {
		ok     bool
		closed bool
	}
	reloadMsg  struct{}
	tickMsg    struct{}
	reachedMsg struct {
		id  string
		err error
	}
	doneMsg struct {
		text    string // what to say when it worked
		err     error
		stay    bool   // keep the menus open (a setting changed in place)
		select_ string // the row to move to
	}
	execDoneMsg struct {
		what string
		err  error
		then func(m *Model)
	}
)

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.reload(true), m.watch(m.backend.Watch(m.ctx)), tick())
}

func tick() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

// reload reads everything again. probe also tries the remote environments.
func (m *Model) reload(probe bool) tea.Cmd {
	m.generation++
	generation := m.generation
	return func() tea.Msg {
		snap, err := m.backend.Load(m.ctx)
		return loadedMsg{generation: generation, snap: snap, err: err, probe: probe}
	}
}

// watch waits for the next change the engine reports.
func (m *Model) watch(changes <-chan bool) tea.Cmd {
	return func() tea.Msg {
		ok, open := <-changes
		if !open {
			return changedMsg{closed: true}
		}
		return watchedMsg{changedMsg: changedMsg{ok: ok}, changes: changes}
	}
}

type watchedMsg struct {
	changedMsg
	changes <-chan bool
}

func (m *Model) probe(id, name string) tea.Cmd {
	return func() tea.Msg { return reachedMsg{id: id, err: m.backend.Reach(m.ctx, name)} }
}

// Dialog stack.
func (m *Model) push(d modal) { m.modals = append(m.modals, d) }

func (m *Model) pop() {
	if len(m.modals) > 0 {
		m.modals = m.modals[:len(m.modals)-1]
	}
}

func (m *Model) closeAll() { m.modals = nil }

func (m *Model) top() modal {
	if len(m.modals) == 0 {
		return nil
	}
	return m.modals[len(m.modals)-1]
}

// say puts a message in the line at the bottom.
func (m *Model) say(c color, format string, args ...any) {
	m.note = &note{color: c, text: fmt.Sprintf(format, args...)}
}

// selected is the row the cursor is on.
func (m *Model) selected() *row {
	if index := indexOf(m.rows, m.cursor); index >= 0 {
		return &m.rows[index]
	}
	return nil
}

// env finds an environment of the current snapshot by id.
func (m *Model) env(id string) *EnvState {
	for i := range m.snap.Envs {
		if m.snap.Envs[i].Env.ID == id {
			return &m.snap.Envs[i]
		}
	}
	return nil
}

// do runs work in the background behind a busy dialog. When it ends the
// dialogs close and text is shown — or, with stay, only the busy dialog
// closes and the menu under it shows the new values.
func (m *Model) do(title, text string, stay bool, work func(progress io.Writer) error) tea.Cmd {
	return m.doThen(title, stay, func(progress io.Writer) (string, string, error) {
		return text, "", work(progress)
	})
}

// doThen is do for work that decides its own closing text and which row to
// select afterwards.
func (m *Model) doThen(title string, stay bool, work func(progress io.Writer) (text, selectID string, err error)) tea.Cmd {
	output := &progress{}
	m.push(&busy{name: title, progress: output})
	return func() tea.Msg {
		text, selectID, err := work(output)
		if err != nil {
			// The last lines of output usually say more than the error.
			if tail := output.tail(15); len(tail) > 0 {
				err = fmt.Errorf("%w\n\n%s", err, strings.Join(tail, "\n"))
			}
		}
		return doneMsg{text: text, err: err, stay: stay, select_: selectID}
	}
}

// shellOut hands the terminal to a program and comes back when it ends.
// failed says whether the way it ended is worth a message (nil: any error
// is) — after the return the full screen covers whatever the program
// printed, so a failure would otherwise look like nothing happened.
func (m *Model) shellOut(what string, command *exec.Cmd, err error, failed func(error) bool, then func(m *Model)) tea.Cmd {
	m.closeAll()
	if err != nil {
		m.say(red, "%s", err.Error())
		return nil
	}
	return tea.ExecProcess(command, func(err error) tea.Msg {
		if err != nil && failed != nil && !failed(err) {
			err = nil
		}
		return execDoneMsg{what: what, err: err, then: then}
	})
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height

	case tea.KeyPressMsg:
		return m, m.keyPressed(msg)

	case tea.PasteMsg:
		if d, ok := m.top().(*input); ok {
			d.paste(msg.Content)
		}

	case tickMsg:
		m.frame++
		return m, tea.Batch(tick(), m.tendLogs())

	case loadedMsg:
		return m, m.loadedNow(msg)

	case watchedMsg:
		m.degraded = !msg.ok
		next := m.watch(msg.changes)
		if m.pending {
			return m, next
		}
		// A start or a stop is a burst of events; one reload covers them.
		m.pending = true
		return m, tea.Batch(next, tea.Tick(300*time.Millisecond, func(time.Time) tea.Msg { return reloadMsg{} }))

	case changedMsg: // the watch ended with the program
	case reloadMsg:
		m.pending = false
		if _, working := m.top().(*busy); working {
			return m, nil // the action in progress reloads when it ends
		}
		return m, m.reload(false)

	case reachedMsg:
		state := reachState{checked: true}
		if msg.err != nil {
			state.problem = msg.err.Error()
		}
		m.reach[msg.id] = state
		m.rows = buildRows(m.snap, m.reach)

	case doneMsg:
		if _, working := m.top().(*busy); working {
			m.pop()
		}
		switch {
		case msg.err != nil:
			m.closeAll()
			m.say(red, "%s", msg.err.Error())
		case msg.stay:
			if msg.text != "" {
				m.say(green, "%s", msg.text)
			}
		default:
			m.closeAll()
			if msg.text != "" {
				m.say(green, "%s", msg.text)
			}
		}
		if msg.select_ != "" {
			m.cursor = msg.select_
		}
		cmd := m.reload(false)
		m.own = m.generation
		return m, cmd

	case execDoneMsg:
		if msg.err != nil {
			m.say(red, "%s failed (%v) — its output is in the terminal behind this screen", msg.what, msg.err)
		}
		if msg.then != nil {
			msg.then(m)
		}
		return m, m.reload(false)
	}
	return m, nil
}

// loadedNow takes a finished reload in.
func (m *Model) loadedNow(msg loadedMsg) tea.Cmd {
	if msg.generation != m.generation {
		return nil // a newer reload is on its way
	}
	if msg.err != nil {
		// What is shown stays; the next reload may work again (a settings
		// file being edited, the engine restarting).
		m.say(red, "%s", msg.err.Error())
		m.loaded = true
		if m.rows == nil {
			m.rows = buildRows(m.snap, m.reach)
		}
		return nil
	}
	before := m.snap
	m.snap, m.loaded = msg.snap, true
	m.rows = buildRows(m.snap, m.reach)
	if index := indexOf(m.rows, m.cursor); index >= 0 {
		m.cursor = m.rows[index].id
	}
	if change := stateChanges(before, m.snap); change != "" && msg.generation != m.own && (m.note == nil || m.note.color != red) && m.top() == nil {
		m.say(plain, "%s", change)
	}

	var cmds []tea.Cmd
	for _, state := range m.snap.Envs {
		if state.Env.Local() {
			continue
		}
		if known := m.reach[state.Env.ID]; msg.probe || (!known.checked && !known.checking) {
			m.reach[state.Env.ID] = reachState{checking: true}
			cmds = append(cmds, m.probe(state.Env.ID, state.Env.Name))
		}
	}
	m.rows = buildRows(m.snap, m.reach)
	return tea.Batch(cmds...)
}

// stateChanges describes what changed between two snapshots without anyone
// here having asked for it: one line, or a count.
func stateChanges(before, after Snapshot) string {
	old := map[string]ops.State{}
	for _, state := range before.Envs {
		if state.Env.Local() && state.Known {
			old[state.Env.ID] = state.Status.State()
		}
	}
	var changes []string
	for _, state := range after.Envs {
		if !state.Env.Local() || !state.Known {
			continue
		}
		if was, seen := old[state.Env.ID]; seen && was != state.Status.State() {
			changes = append(changes, fmt.Sprintf("%s: %s → %s", state.Env.Name, was, state.Status.State()))
		}
	}
	switch len(changes) {
	case 0:
		return ""
	case 1:
		return changes[0]
	}
	return fmt.Sprintf("%d environments changed state", len(changes))
}

func (m *Model) keyPressed(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()
	if key == "ctrl+c" {
		return tea.Quit
	}
	if d := m.top(); d != nil {
		return d.key(m, key, msg)
	}

	// The list. Any key takes the last message away, except the one that
	// opens it in full.
	said := m.note
	if key != "m" {
		m.note = nil
	}
	index := indexOf(m.rows, m.cursor)
	switch key {
	case "up", "k":
		if index >= 0 {
			m.cursor = m.rows[step(m.rows, index, -1)].id
		}
	case "down", "j":
		if index >= 0 {
			m.cursor = m.rows[step(m.rows, index, 1)].id
		}
	case "r":
		return m.reload(true)
	case "m":
		if said != nil {
			m.push(&reader{name: "Message", text: said.text})
		}
	case "enter":
		if index >= 0 {
			return m.choose(m.rows[index])
		}
	case "q", "esc":
		m.push(&confirm{name: "Quit", text: []string{"Leave bh? Running environments keep running."},
			onYes: func(*Model) tea.Cmd { return tea.Quit }})
	}
	return nil
}

// logTail follows the output of the selected environment.
type logTail struct {
	id      string
	cancel  context.CancelFunc
	lock    sync.Mutex
	lines   []string
	rest    string
	ended   bool
	endedAt time.Time
}

func (l *logTail) Write(data []byte) (int, error) {
	l.lock.Lock()
	defer l.lock.Unlock()
	parts := strings.Split(l.rest+string(data), "\n")
	l.rest = parts[len(parts)-1]
	for _, line := range parts[:len(parts)-1] {
		l.lines = append(l.lines, sanitize(strings.TrimRight(line, "\r")))
	}
	if len(l.lines) > 200 {
		l.lines = l.lines[len(l.lines)-200:]
	}
	return len(data), nil
}

func (l *logTail) tail(n int) []string {
	l.lock.Lock()
	defer l.lock.Unlock()
	if n <= 0 {
		return nil
	}
	if len(l.lines) > n {
		return append([]string{}, l.lines[len(l.lines)-n:]...)
	}
	return append([]string{}, l.lines...)
}

func (l *logTail) finished() (bool, time.Time) {
	l.lock.Lock()
	defer l.lock.Unlock()
	return l.ended, l.endedAt
}

// tendLogs keeps one log stream open: for the selected local environment,
// while it has containers. A stream that ends while the environment runs is
// opened again after a pause; an environment without containers is left
// alone, so that nothing is started over and over for it.
func (m *Model) tendLogs() tea.Cmd {
	var want *EnvState
	if selected := m.selected(); selected != nil && selected.kind == rowEnv && selected.env.Env.Local() && selected.env.Known {
		if selected.env.Status.Holder != nil || selected.env.Status.Dev != nil {
			want = selected.env
		}
	}
	if m.logs != nil {
		if selected := m.selected(); selected == nil || selected.kind != rowEnv || selected.env.Env.ID != m.logs.id {
			// Another row: the old stream and what it showed go.
			m.logs.cancel()
			m.logs = nil
		}
	}
	if want == nil {
		if m.logs != nil {
			m.logs.cancel() // stopped and removed: keep what is shown
		}
		return nil
	}
	if m.logs != nil {
		ended, at := m.logs.finished()
		if !ended || !active(*want) || time.Since(at) < 3*time.Second {
			return nil
		}
	}
	return m.follow(want)
}

func (m *Model) follow(state *EnvState) tea.Cmd {
	ctx, cancel := context.WithCancel(m.ctx)
	stream := &logTail{id: state.Env.ID, cancel: cancel}
	if m.logs != nil {
		m.logs.cancel()
	}
	m.logs = stream
	name := state.Env.Name
	return func() tea.Msg {
		_ = m.backend.FollowLogs(ctx, name, 20, stream)
		stream.lock.Lock()
		stream.ended, stream.endedAt = true, time.Now()
		stream.lock.Unlock()
		return nil
	}
}

// active: one of the containers runs.
func active(state EnvState) bool {
	return (state.Status.Holder != nil && state.Status.Holder.Running()) ||
		(state.Status.Dev != nil && state.Status.Dev.Running())
}

// devRunning: the environment can be connected to.
func devRunning(state EnvState) bool {
	return state.Status.Dev != nil && state.Status.Dev.Running()
}

// created: the environment has containers.
func created(state EnvState) bool {
	return state.Status.Holder != nil || state.Status.Dev != nil
}
