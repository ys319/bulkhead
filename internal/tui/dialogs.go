package tui

import (
	"fmt"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
)

// modal is a dialog over the list. Dialogs stack: a menu opens a submenu or
// a question, and Esc goes back one level.
type modal interface {
	title() string
	// lines is the content, for a dialog whose inside is inner cells wide.
	lines(m *Model, inner int) []string
	// key handles a key press.
	key(m *Model, key string, msg tea.KeyPressMsg) tea.Cmd
	// hint is the line of keys shown at the bottom while the dialog is open.
	hint() string
}

// item is a line of a menu.
type item struct {
	label string
	// value is the current setting, shown at the right edge so that the
	// label never changes width.
	value string
	color color
	// disabled is why the item cannot be used now. It is still shown, in
	// gray, so that its value can be read; choosing it shows the reason.
	disabled  string
	separator bool
	// run is what Enter does.
	run func(m *Model) tea.Cmd
	// cycle changes the value in place (Space, ← →); Enter does the same
	// when there is no run.
	cycle func(direction int)
}

// menu is a list of choices. Its items are built anew every time they are
// shown, so values always reflect what is on disk and in Docker now.
type menu struct {
	name string
	note []string // lines above the items
	// form: the items hold values that change in place.
	form   bool
	build  func(m *Model) []item
	cursor int
}

func (d *menu) title() string { return d.name }

// items returns the current items and moves the cursor onto a line that can
// be chosen.
func (d *menu) items(m *Model) []item {
	items := d.build(m)
	if len(items) == 0 {
		return items
	}
	if d.cursor >= len(items) {
		d.cursor = len(items) - 1
	}
	for d.cursor < len(items)-1 && items[d.cursor].separator {
		d.cursor++
	}
	for d.cursor > 0 && items[d.cursor].separator {
		d.cursor--
	}
	return items
}

func (d *menu) lines(m *Model, inner int) []string {
	var lines []string
	for _, note := range d.note {
		lines = append(lines, wrap(note, inner)...)
	}
	if len(d.note) > 0 {
		lines = append(lines, "")
	}
	for i, it := range d.items(m) {
		if it.separator {
			lines = append(lines, dim(strings.Repeat("─", inner)))
			continue
		}
		look := style{color: it.color}
		if it.disabled != "" {
			look = style{color: gray}
		}
		marker := "  "
		if i == d.cursor {
			marker, look.bold = "❯ ", true
		}
		label := it.label
		if gap := inner - 2 - width(it.label) - width(it.value); it.value != "" && gap >= 1 {
			label += strings.Repeat(" ", gap) + it.value
		}
		lines = append(lines, marker+look.render(label))
	}
	return lines
}

func (d *menu) move(items []item, direction int) {
	for i := d.cursor + direction; i >= 0 && i < len(items); i += direction {
		if !items[i].separator {
			d.cursor = i
			return
		}
	}
}

func (d *menu) key(m *Model, key string, _ tea.KeyPressMsg) tea.Cmd {
	items := d.items(m)
	if len(items) == 0 {
		m.pop()
		return nil
	}
	current := items[d.cursor]
	switch key {
	case "up", "k":
		d.move(items, -1)
	case "down", "j":
		d.move(items, 1)
	case "esc":
		m.pop()
	case "space", "left", "right", "enter":
		if current.disabled != "" {
			m.say(yellow, "%s", current.disabled)
			return nil
		}
		if current.cycle != nil && (key != "enter" || current.run == nil) {
			direction := 1
			if key == "left" {
				direction = -1
			}
			current.cycle(direction)
			return nil
		}
		if key == "enter" && current.run != nil {
			return current.run(m)
		}
	}
	return nil
}

func (d *menu) hint() string {
	if d.form {
		return "↑↓ move · space change · enter choose · esc back"
	}
	return "↑↓ move · enter choose · esc back"
}

// confirm asks a yes/no question. No is where the cursor starts.
type confirm struct {
	name  string
	text  []string
	color color // of Yes: red for what destroys, yellow for what disrupts
	yes   bool
	onYes func(m *Model) tea.Cmd
}

func (d *confirm) title() string { return d.name }

func (d *confirm) lines(_ *Model, inner int) []string {
	var lines []string
	for _, paragraph := range d.text {
		lines = append(lines, wrap(paragraph, inner)...)
	}
	choice := func(label string, chosen bool, c color) string {
		if chosen {
			return "❯ " + style{color: c, bold: true}.render(label)
		}
		return "  " + label
	}
	return append(lines, "", choice("No", !d.yes, plain)+"    "+choice("Yes", d.yes, d.color))
}

func (d *confirm) key(m *Model, key string, _ tea.KeyPressMsg) tea.Cmd {
	switch key {
	case "left", "right", "up", "down", "tab", "h", "l":
		d.yes = !d.yes
	case "esc", "n":
		m.pop()
	case "y":
		d.yes = true
	case "enter":
		if !d.yes {
			m.pop()
			return nil
		}
		return d.onYes(m)
	}
	return nil
}

func (d *confirm) hint() string { return "←→ select · enter confirm · esc back" }

// input asks for a line of text.
type input struct {
	name   string
	prompt []string
	value  []rune
	secret bool // show dots instead of the text (tokens)
	// problem is what was wrong with the last attempt; it stays in the
	// dialog so the text can be corrected.
	problem string
	// submit takes the text. An error keeps the dialog open.
	submit func(m *Model, value string) (tea.Cmd, error)
	// extra handles keys of its own (ctrl+o and the like).
	extra func(m *Model, key string) (tea.Cmd, bool)
	hints string
}

func (d *input) title() string { return d.name }

func (d *input) lines(_ *Model, inner int) []string {
	var lines []string
	for _, paragraph := range d.prompt {
		lines = append(lines, wrap(paragraph, inner)...)
	}
	shown := sanitize(strings.ReplaceAll(string(d.value), "\n", " "))
	if d.secret {
		shown = strings.Repeat("•", len(d.value))
	}
	// The end of the text is what is being typed: keep it in view.
	if room := inner - 3; width(shown) > room && room > 1 {
		runes := []rune(shown)
		shown = "…" + string(runes[len(runes)-(room-1):])
	}
	lines = append(lines, "", "> "+shown+"▏")
	if d.problem != "" {
		lines = append(lines, "")
		for _, line := range wrap(d.problem, inner) {
			lines = append(lines, paint(red, line))
		}
	}
	return lines
}

func (d *input) key(m *Model, key string, msg tea.KeyPressMsg) tea.Cmd {
	if d.extra != nil {
		if cmd, handled := d.extra(m, key); handled {
			return cmd
		}
	}
	switch key {
	case "esc":
		m.pop()
	case "enter":
		cmd, err := d.submit(m, string(d.value))
		if err != nil {
			d.problem = sanitize(err.Error())
			return nil
		}
		return cmd
	case "backspace":
		if len(d.value) > 0 {
			d.value = d.value[:len(d.value)-1]
		}
	case "ctrl+u":
		d.value = nil
	case "space":
		d.value = append(d.value, ' ')
	default:
		if msg.Text != "" {
			d.value = append(d.value, []rune(msg.Text)...)
		}
	}
	return nil
}

// paste adds pasted text as it is; whether it is acceptable is the
// submit's business (a pasted private key must be refused, not trimmed
// into something that looks fine).
func (d *input) paste(text string) { d.value = append(d.value, []rune(text)...) }

func (d *input) hint() string {
	if d.hints != "" {
		return d.hints
	}
	return "type or paste · enter confirm · esc back"
}

// progress collects the output of work in progress; the busy dialog shows
// its last lines. Written from the working goroutine, read when drawing.
type progress struct {
	lock  sync.Mutex
	lines []string
	rest  string
}

func (p *progress) Write(data []byte) (int, error) {
	p.lock.Lock()
	defer p.lock.Unlock()
	text := p.rest + string(data)
	parts := strings.Split(text, "\n")
	p.rest = parts[len(parts)-1]
	for _, line := range parts[:len(parts)-1] {
		// A carriage return redraws a line (docker build's progress): keep
		// what it ended up as.
		if at := strings.LastIndex(line, "\r"); at >= 0 {
			line = line[at+1:]
		}
		if line = strings.TrimSpace(sanitize(line)); line != "" {
			p.lines = append(p.lines, line)
		}
	}
	if len(p.lines) > 200 {
		p.lines = p.lines[len(p.lines)-200:]
	}
	return len(data), nil
}

// tail returns the last n lines.
func (p *progress) tail(n int) []string {
	p.lock.Lock()
	defer p.lock.Unlock()
	if len(p.lines) > n {
		return append([]string{}, p.lines[len(p.lines)-n:]...)
	}
	return append([]string{}, p.lines...)
}

// busy is shown while something runs. It cannot be closed.
type busy struct {
	name     string
	progress *progress
}

func (d *busy) title() string { return d.name }

func (d *busy) lines(m *Model, inner int) []string {
	lines := []string{spinner[m.frame%len(spinner)] + " working"}
	// Always three lines, so the dialog does not change size with every
	// line of output.
	shown := d.progress.tail(3)
	for len(shown) < 3 {
		shown = append(shown, "")
	}
	for _, line := range shown {
		lines = append(lines, dim(fit(line, inner)))
	}
	return lines
}

func (d *busy) key(*Model, string, tea.KeyPressMsg) tea.Cmd { return nil }
func (d *busy) hint() string                                { return "" }

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// reader shows a text in full, scrolling when it is long: the place to read
// an error that the one-line message area cut short.
type reader struct {
	name   string
	text   string
	offset int
	rows   int
}

func (d *reader) title() string { return d.name }

func (d *reader) lines(m *Model, inner int) []string {
	all := wrap(sanitizeLines(d.text), inner)
	d.rows = m.height - 8
	if d.rows < 3 {
		d.rows = 3
	}
	if d.offset > len(all)-d.rows {
		d.offset = len(all) - d.rows
	}
	if d.offset < 0 {
		d.offset = 0
	}
	end := d.offset + d.rows
	if end > len(all) {
		end = len(all)
	}
	lines := append([]string{}, all[d.offset:end]...)
	if len(all) > d.rows {
		lines = append(lines, dim(fmt.Sprintf("lines %d-%d of %d", d.offset+1, end, len(all))))
	}
	return lines
}

func (d *reader) key(m *Model, key string, _ tea.KeyPressMsg) tea.Cmd {
	switch key {
	case "up", "k":
		d.offset--
	case "down", "j":
		d.offset++
	case "esc", "enter", "q":
		m.pop()
	}
	return nil
}

func (d *reader) hint() string { return "↑↓ scroll · esc close" }

// sanitizeLines is sanitize for a text of several lines.
func sanitizeLines(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = sanitize(line)
	}
	return strings.Join(lines, "\n")
}
