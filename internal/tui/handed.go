package tui

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/ys319/bulkhead/internal/config"
)

// What an environment is handed, in three menus for three meanings:
// Variables (plain values, shown), Secrets (values of the user's, never
// shown) and Tokens (credentials a service issues for the environment,
// which bh leads to, checks, renews and revokes).

// variablesMenu lists the variables with their values; choosing one changes
// or removes it.
func (m *Model) variablesMenu(id string) *menu {
	return &menu{name: "Variables", note: []string{"Environment variables that are no secret. New shells see a change."}, build: func(m *Model) []item {
		state := m.env(id)
		if state == nil {
			return nil
		}
		name := state.Env.Name
		keys := make([]string, 0, len(state.Env.Vars))
		for key := range state.Env.Vars {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var items []item
		for _, key := range keys {
			items = append(items, item{label: key, value: fit(sanitize(state.Env.Vars[key]), 40), run: func(m *Model) tea.Cmd {
				m.push(&menu{name: key, build: func(*Model) []item {
					return []item{
						{label: "Change the value", run: func(m *Model) tea.Cmd { return m.askVarValue(name, key) }},
						{label: "Remove", color: yellow, run: func(m *Model) tea.Cmd {
							return m.doThen("Removing "+key, true, func(io.Writer) (string, string, error) {
								text, err := m.backend.UnsetVar(m.ctx, name, key)
								return text, "", err
							})
						}},
					}
				}})
				return nil
			}})
		}
		return append(items, item{label: "+ Add variable", run: func(m *Model) tea.Cmd {
			m.push(&input{name: "Add variable", prompt: []string{"Its name (for example NODE_ENV):"},
				submit: func(m *Model, value string) (tea.Cmd, error) {
					key := strings.TrimSpace(value)
					if err := config.CheckName(key); err != nil {
						return nil, err
					}
					m.pop()
					return m.askVarValue(name, key), nil
				}})
			return nil
		}})
	}}
}

func (m *Model) askVarValue(name, key string) tea.Cmd {
	m.push(&input{name: key, prompt: []string{"The value of " + key + " (one line):"},
		submit: func(m *Model, value string) (tea.Cmd, error) {
			if err := config.CheckValue(value); err != nil {
				return nil, err
			}
			return m.doThen("Saving "+key, false, func(io.Writer) (string, string, error) {
				text, err := m.backend.SetVar(m.ctx, name, key, value)
				return text, "", err
			}), nil
		}})
	return nil
}

// secretsMenu lists the secrets by name; their values are never shown.
func (m *Model) secretsMenu(id string) *menu {
	return &menu{name: "Secrets", note: []string{"Values of yours the environment gets as environment variables. They are kept in this machine's credential store and never shown."}, build: func(m *Model) []item {
		state := m.env(id)
		if state == nil {
			return nil
		}
		name := state.Env.Name
		var items []item
		for _, key := range state.Secrets {
			items = append(items, item{label: key, run: func(m *Model) tea.Cmd {
				m.push(&menu{name: key, build: func(*Model) []item {
					return []item{
						{label: "Replace the value", run: func(m *Model) tea.Cmd { return m.askSecretValue(name, key) }},
						{label: "Remove", color: yellow, run: func(m *Model) tea.Cmd {
							m.push(&confirm{name: "Remove " + key, color: yellow,
								text: []string{fmt.Sprintf("Take %s out of %q and forget its value?", key, name)},
								onYes: func(m *Model) tea.Cmd {
									return m.doThen("Removing "+key, false, func(io.Writer) (string, string, error) {
										text, err := m.backend.RemoveSecret(m.ctx, name, key)
										return text, "", err
									})
								}})
							return nil
						}},
					}
				}})
				return nil
			}})
		}
		return append(items, item{label: "+ Add secret", run: func(m *Model) tea.Cmd {
			m.push(&input{name: "Add secret", prompt: []string{"The environment variable it comes as (for example OPENAI_API_KEY):"},
				submit: func(m *Model, value string) (tea.Cmd, error) {
					key := strings.TrimSpace(value)
					if err := config.CheckName(key); err != nil {
						return nil, err
					}
					m.pop()
					return m.askSecretValue(name, key), nil
				}})
			return nil
		}})
	}}
}

func (m *Model) askSecretValue(name, key string) tea.Cmd {
	m.push(&input{name: key, secret: true, prompt: []string{"Paste the value of " + key + ":"},
		submit: func(m *Model, value string) (tea.Cmd, error) {
			value = strings.TrimSpace(value)
			if err := config.CheckValue(value); err != nil {
				return nil, err
			}
			return m.doThen("Saving "+key, false, func(io.Writer) (string, string, error) {
				text, err := m.backend.SetSecret(m.ctx, name, key, value)
				return text, "", err
			}), nil
		}})
	return nil
}

// tokensMenu lists the services a token can be had from, each with the
// token the environment has from it.
func (m *Model) tokensMenu(id string) *menu {
	return &menu{name: "Tokens", note: []string{"Credentials a service issues for this environment. Everything inside can read them: what a token may do is the only protection."}, build: func(m *Model) []item {
		state := m.env(id)
		if state == nil {
			return nil
		}
		var items []item
		for _, service := range m.backend.TokenServices() {
			value := "none"
			if token := tokenOf(*state, service.Name); token != nil {
				value = fit(sanitize(token.Summary), 44)
			}
			items = append(items, item{label: service.Title, value: value, run: func(m *Model) tea.Cmd {
				m.push(m.tokenMenu(id, service))
				return nil
			}})
		}
		return items
	}}
}

func tokenOf(state EnvState, service string) *Token {
	for i := range state.Tokens {
		if state.Tokens[i].Service == service {
			return &state.Tokens[i]
		}
	}
	return nil
}

// tokenMenu is the token of one service: get one, or — when there is one —
// refresh it (the same again, new value), replace it (another one), remove
// it (revoked).
func (m *Model) tokenMenu(id string, service TokenService) *menu {
	return &menu{name: service.Title + " token", build: func(m *Model) []item {
		state := m.env(id)
		if state == nil {
			return nil
		}
		name := state.Env.Name
		get := func(label string) item {
			return item{label: label, run: func(m *Model) tea.Cmd {
				if len(service.Presets) == 0 {
					return m.askToken(name, service, "", false)
				}
				m.push(m.presetMenu(name, service))
				return nil
			}}
		}
		token := tokenOf(*state, service.Name)
		if token == nil {
			return []item{get("Get a token")}
		}
		return []item{
			{label: sanitize(token.Summary), disabled: "this is the token in use"},
			separator,
			{label: "Refresh", value: "same token, new value", run: func(m *Model) tea.Cmd {
				return m.askToken(name, service, "", true)
			}},
			get("Replace"),
			{label: "Remove", color: red, run: func(m *Model) tea.Cmd {
				m.push(&confirm{name: "Remove the " + service.Title + " token", color: red,
					text: []string{fmt.Sprintf("Revoke the %s token of %q and take it out of the environment?", service.Title, name), "What uses it inside loses its access at once."},
					onYes: func(m *Model) tea.Cmd {
						return m.doThen("Revoking", false, func(io.Writer) (string, string, error) {
							text, err := m.backend.RemoveToken(m.ctx, name, service.Name)
							return text, "", err
						})
					}})
				return nil
			}},
		}
	}}
}

// presetMenu asks what the token may do. No choice is preselected: it
// decides what code in the environment can do outside it.
func (m *Model) presetMenu(name string, service TokenService) *menu {
	return &menu{name: "What may code in " + name + " do on " + service.Title + "?", note: []string{"Each includes the ones above it."}, build: func(*Model) []item {
		var items []item
		for _, preset := range service.Presets {
			items = append(items, item{label: preset.Name, value: preset.About, run: func(m *Model) tea.Cmd {
				m.pop()
				return m.askToken(name, service, preset.Name, false)
			}})
		}
		return items
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

// askToken opens the service's page and waits for the token made there.
// refresh: the token the environment has, regenerated.
func (m *Model) askToken(name string, service TokenService, preset string, refresh bool) tea.Cmd {
	issue, err := m.backend.TokenIssue(name, service.Name, preset, refresh)
	if err != nil {
		return m.fail(err)
	}
	prompt := append(append([]string{}, issue.Steps...), issue.Address, "Then paste the token here:")
	dialog := &input{name: service.Title + " token", secret: true, hints: pasteHints, extra: pageKeys(issue.Address), prompt: prompt,
		submit: func(m *Model, value string) (tea.Cmd, error) {
			value = strings.TrimSpace(value)
			if value == "" {
				return nil, errors.New("paste the token the page shows")
			}
			return m.doThen("Checking the token with "+service.Title, false, func(io.Writer) (string, string, error) {
				text, err := m.backend.SetToken(m.ctx, name, service.Name, preset, value, issue.TokenName, refresh)
				return text, "", err
			}), nil
		}}
	if err := m.backend.OpenURL(issue.Address); err != nil {
		dialog.problem = "the browser did not open — ctrl+y copies the address"
	}
	m.push(dialog)
	return nil
}

// handedLines are the section of the detail pane about what the
// environment is handed: each token in a line (coloured when it is about
// to expire or has), secrets and variables by name.
func handedLines(state EnvState, inner int) []string {
	var lines []string
	for _, token := range state.Tokens {
		summary := sanitize(token.Summary)
		switch token.Urgency {
		case "expired":
			summary = paint(red, summary)
		case "soon":
			summary = paint(yellow, summary)
		}
		lines = append(lines, field(inner, "  "+token.Service, summary)...)
	}
	if len(state.Secrets) > 0 {
		lines = append(lines, field(inner, "  secrets", sanitize(strings.Join(state.Secrets, ", ")))...)
	}
	if len(state.Env.Vars) > 0 {
		keys := make([]string, 0, len(state.Env.Vars))
		for key := range state.Env.Vars {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		lines = append(lines, field(inner, "  variables", strings.Join(keys, ", "))...)
	}
	if state.TokenProblem != "" {
		lines = append(lines, field(inner, "  problem", paint(red, sanitize(firstLine(state.TokenProblem))))...)
	}
	if len(lines) == 0 {
		return nil
	}
	return append([]string{bold("Handed in")}, lines...)
}
