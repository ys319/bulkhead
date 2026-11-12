package tui

import (
	"errors"
	"fmt"
	"io"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/doctor"
)

// kindLabel is how a kind of token is called on screen.
func kindLabel(kind string) string {
	switch kind {
	case "github":
		return "GitHub"
	case config.TokenKindHF:
		return "Hugging Face"
	}
	return "custom"
}

// tokensMenu: GitHub first (it has a screen of its own: bh issues and
// revokes that one), then the tokens that are there, then what can be added.
func (m *Model) tokensMenu(id string) *menu {
	return &menu{name: "Tokens", build: func(m *Model) []item {
		state := m.env(id)
		if state == nil {
			return nil
		}
		name := state.Env.Name
		var github *Token
		hasHF := false
		for i := range state.Tokens {
			switch state.Tokens[i].Kind {
			case "github":
				github = &state.Tokens[i]
			case config.TokenKindHF:
				hasHF = true
			}
		}
		value := "none"
		if github != nil {
			value = "set"
		}
		items := []item{{label: "GitHub", value: value, run: func(m *Model) tea.Cmd {
			m.push(m.githubMenu(id))
			return nil
		}}}
		for _, token := range state.Tokens {
			if token.Kind == "github" {
				continue
			}
			items = append(items, item{label: token.Name, value: kindLabel(token.Kind), run: func(m *Model) tea.Cmd {
				m.push(m.tokenMenu(name, token))
				return nil
			}})
		}
		// One Hugging Face token: it has one variable name.
		if !hasHF {
			items = append(items, item{label: "+ Add Hugging Face token", run: func(m *Model) tea.Cmd {
				return m.askTokenValue(name, config.TokenKindHF, config.HFTokenName, true)
			}})
		}
		return append(items, item{label: "+ Add custom token", run: func(m *Model) tea.Cmd {
			m.push(&input{name: "Add custom token", prompt: []string{"The environment variable it is exported as (for example OPENAI_API_KEY):"},
				submit: func(m *Model, value string) (tea.Cmd, error) {
					variable := strings.TrimSpace(value)
					if err := config.CheckTokenName(config.TokenKindCustom, variable); err != nil {
						return nil, err
					}
					m.pop()
					return m.askTokenValue(name, config.TokenKindCustom, variable, false), nil
				}})
			return nil
		}})
	}}
}

// tokenMenu is what can be done with a token that is there.
func (m *Model) tokenMenu(name string, token Token) *menu {
	return &menu{name: token.Name, build: func(*Model) []item {
		return []item{
			{label: "Replace", run: func(m *Model) tea.Cmd {
				return m.askTokenValue(name, token.Kind, token.Name, false)
			}},
			// Yellow, not red: nothing is destroyed, but the token stays
			// valid where it was issued, which the question says.
			{label: "Remove", color: yellow, run: func(m *Model) tea.Cmd {
				m.push(&confirm{name: "Remove " + token.Name, color: yellow,
					text: []string{fmt.Sprintf("Take %s out of %q?", token.Name, name), "bh cannot revoke it: it stays valid at its service until you revoke it there."},
					onYes: func(m *Model) tea.Cmd {
						return m.doThen("Removing "+token.Name, false, func(io.Writer) (string, string, error) {
							text, err := m.backend.RemoveToken(m.ctx, name, token.Name)
							return text, "", err
						})
					}})
				return nil
			}},
		}
	}}
}

// pasteHints are the keys of a dialog that waits for a token made on a web
// page.
const pasteHints = "paste · enter confirm · ctrl+o open the page · ctrl+y copy its address · esc back"

// pageKeys are those keys: open the page again, or copy its address — for
// a browser on another machine than the one bh runs on.
func pageKeys(address string) func(m *Model, key string) (tea.Cmd, bool) {
	return func(m *Model, key string) (tea.Cmd, bool) {
		switch key {
		case "ctrl+o":
			if err := m.backend.OpenURL(address); err != nil {
				m.say(yellow, "the browser did not open (%v) — ctrl+y copies the address", err)
			}
			return nil, true
		case "ctrl+y":
			m.say(plain, "sent the address to the clipboard")
			return tea.SetClipboard(address), true
		}
		return nil, false
	}
}

// askTokenValue asks for the value of a token, shown as dots. With open,
// the page where such a token is issued is opened first.
func (m *Model) askTokenValue(name, kind, variable string, open bool) tea.Cmd {
	dialog := &input{name: variable, secret: true, prompt: []string{"Paste the value of " + variable + ":"},
		submit: func(m *Model, value string) (tea.Cmd, error) {
			value = strings.TrimSpace(value)
			if err := config.CheckTokenValue(value); err != nil {
				return nil, err
			}
			return m.doThen("Saving "+variable, false, func(io.Writer) (string, string, error) {
				text, err := m.backend.SetToken(m.ctx, name, kind, variable, value)
				return text, "", err
			}), nil
		}}
	if address := m.backend.TokenPage(kind); address != "" {
		dialog.prompt = []string{"Create a token on the page (" + address + "), then paste it here:"}
		dialog.extra, dialog.hints = pageKeys(address), pasteHints
		if open {
			if err := m.backend.OpenURL(address); err != nil {
				dialog.problem = "the browser did not open — ctrl+y copies the address"
			}
		}
	}
	m.push(dialog)
	return nil
}

// githubMenu is the GitHub token of an environment: bh leads to the page
// that issues it with the right permissions, checks what was pasted, and
// can revoke it.
func (m *Model) githubMenu(id string) *menu {
	return &menu{name: "GitHub token", build: func(m *Model) []item {
		state := m.env(id)
		if state == nil {
			return nil
		}
		name := state.Env.Name
		issue := func(label string) item {
			return item{label: label, run: func(m *Model) tea.Cmd {
				m.push(m.githubPresets(name))
				return nil
			}}
		}
		for _, token := range state.Tokens {
			if token.Kind != "github" {
				continue
			}
			return []item{
				{label: sanitize(token.Summary), disabled: "this is the token in use — Replace issues a new one"},
				separator,
				issue("Replace"),
				{label: "Revoke", color: red, run: func(m *Model) tea.Cmd {
					m.push(&confirm{name: "Revoke the GitHub token", color: red,
						text: []string{fmt.Sprintf("Revoke the GitHub token of %q at GitHub and take it out of the environment?", name), "git and gh in the environment lose their access at once."},
						onYes: func(m *Model) tea.Cmd {
							return m.doThen("Revoking", false, func(io.Writer) (string, string, error) {
								text, err := m.backend.RevokeGitHub(m.ctx, name)
								return text, "", err
							})
						}})
					return nil
				}},
			}
		}
		return []item{issue("Issue a token")}
	}}
}

// githubPresets asks what the token may do. No choice is preselected: it
// decides what code in the environment can do to the user's repositories.
func (m *Model) githubPresets(name string) *menu {
	about := map[string]string{
		"read":    "read repositories",
		"push":    "read and push",
		"push+pr": "read, push, and open pull requests",
	}
	return &menu{name: "What may the token do?", note: []string{"You choose the repositories on GitHub's page."}, build: func(*Model) []item {
		var items []item
		for _, preset := range config.GitHubPresets {
			items = append(items, item{label: preset, value: about[preset], run: func(m *Model) tea.Cmd {
				address, tokenName, err := m.backend.GitHubIssue(name, preset)
				if err != nil {
					return m.fail(err)
				}
				dialog := &input{name: "GitHub token", secret: true, hints: pasteHints, extra: pageKeys(address),
					prompt: []string{"GitHub's page opens with the form filled in (token name: " + tokenName + ").", "Choose the repositories there, create the token, and paste it here:"},
					submit: func(m *Model, value string) (tea.Cmd, error) {
						value = strings.TrimSpace(value)
						if !config.ValidGitHubToken(value) {
							return nil, errors.New("that does not look like a fine-grained token (github_pat_...)")
						}
						return m.doThen("Checking the token with GitHub", false, func(io.Writer) (string, string, error) {
							text, err := m.backend.SetGitHub(m.ctx, name, value, preset, tokenName)
							return text, "", err
						}), nil
					}}
				if err := m.backend.OpenURL(address); err != nil {
					dialog.problem = "the browser did not open — ctrl+y copies the address"
				}
				m.pop()
				m.push(dialog)
				return nil
			}})
		}
		return items
	}}
}

// tokenLines are the Tokens section of the detail pane: GitHub's in a
// line, the others by name only.
func tokenLines(state EnvState, inner int) []string {
	var lines []string
	byKind := map[string][]string{}
	for _, token := range state.Tokens {
		if token.Kind == "github" {
			lines = append(lines, field(inner, "  github", sanitize(token.Summary))...)
			continue
		}
		byKind[token.Kind] = append(byKind[token.Kind], token.Name)
	}
	for _, kind := range config.TokenKinds {
		if names := byKind[kind]; len(names) > 0 {
			lines = append(lines, field(inner, "  "+kind, strings.Join(names, ", "))...)
		}
	}
	if state.TokenProblem != "" {
		lines = append(lines, field(inner, "  tokens", paint(red, sanitize(firstLine(state.TokenProblem))))...)
	}
	if len(lines) == 0 {
		return nil
	}
	return append([]string{bold("Tokens")}, lines...)
}

// doctorLines sum the findings up for the detail pane. Warnings that a
// setting asks for (the opt-ins) are counted apart: they are there every
// time and are not what "something is off" means. Only what deviates is
// listed.
func doctorLines(findings []doctor.Finding, inner int) []string {
	if len(findings) == 0 {
		return nil
	}
	var passed, optIn int
	var off []doctor.Finding
	skipped := ""
	for _, finding := range findings {
		switch {
		case finding.Standing:
			optIn++
		case finding.Level == doctor.OK:
			passed++
		case finding.Level == doctor.Skip:
			if skipped == "" {
				skipped = finding.Message
			}
		default:
			off = append(off, finding)
		}
	}
	note := ""
	if optIn > 0 {
		note = dim(fmt.Sprintf(" · %d opt-in", optIn))
	}
	if len(off) == 0 {
		if passed == 0 {
			return field(inner, "doctor", dim(sanitize(skipped))+note)
		}
		return field(inner, "doctor", fmt.Sprintf("ok (%d checks)", passed)+note)
	}
	failed := 0
	for _, finding := range off {
		if finding.Level == doctor.Fail {
			failed++
		}
	}
	summary := paint(yellow, fmt.Sprintf("%d warn", len(off)-failed))
	if failed > 0 {
		summary = paint(red, fmt.Sprintf("%d FAIL", failed))
		if len(off) > failed {
			summary += fmt.Sprintf(" / %d warn", len(off)-failed)
		}
	}
	lines := field(inner, "doctor", summary+note)
	for _, finding := range off {
		c := yellow
		if finding.Level == doctor.Fail {
			c = red
		}
		for _, line := range wrap(sanitize(finding.Message), inner-4) {
			lines = append(lines, "    "+paint(c, line))
		}
	}
	return lines
}
