package tui

import (
	"context"
	"errors"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// ErrNothingPicked is returned by Pick when the user leaves without choosing.
var ErrNothingPicked = errors.New("nothing was chosen")

// Choice is a line Pick offers.
type Choice struct {
	Name string
	Note string // shown beside the name, dimmed
}

// picker is a short list under the prompt, in the normal screen: for a
// command that was given no environment.
type picker struct {
	title   string
	choices []Choice
	cursor  int
	picked  string
	done    bool
}

func (p *picker) Init() tea.Cmd { return nil }

func (p *picker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, pressed := msg.(tea.KeyPressMsg)
	if !pressed {
		return p, nil
	}
	switch key.String() {
	case "up", "k":
		if p.cursor > 0 {
			p.cursor--
		}
	case "down", "j":
		if p.cursor < len(p.choices)-1 {
			p.cursor++
		}
	case "enter":
		p.picked, p.done = p.choices[p.cursor].Name, true
		return p, tea.Quit
	case "esc", "q", "ctrl+c":
		p.done = true
		return p, tea.Quit
	}
	return p, nil
}

func (p *picker) View() tea.View {
	if p.done {
		return tea.NewView("")
	}
	lines := []string{bold(p.title)}
	for i, choice := range p.choices {
		line := "  " + choice.Name
		if i == p.cursor {
			line = "❯ " + bold(choice.Name)
		}
		if choice.Note != "" {
			line += dim("  " + sanitize(choice.Note))
		}
		lines = append(lines, line)
	}
	lines = append(lines, dim("↑↓ move · enter choose · esc cancel"))
	return tea.NewView(strings.Join(lines, "\n"))
}

// Pick asks the user to choose one of the choices.
func Pick(ctx context.Context, title string, choices []Choice) (string, error) {
	if len(choices) == 0 {
		return "", ErrNothingPicked
	}
	final, err := tea.NewProgram(&picker{title: title, choices: choices}, tea.WithContext(ctx)).Run()
	if err != nil {
		return "", err
	}
	if chosen := final.(*picker).picked; chosen != "" {
		return chosen, nil
	}
	return "", ErrNothingPicked
}
