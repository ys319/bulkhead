package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Colors carry meaning, not state decoration: red is destructive or broken,
// yellow a warning, green confirms, magenta marks an environment that gave
// up its isolation. The 16 basic colors follow the terminal's own palette.
type color int

const (
	plain color = iota
	red
	green
	yellow
	magenta
	gray
)

var colorCodes = map[color]string{red: "31", green: "32", yellow: "33", magenta: "35", gray: "90"}

// style wraps text in SGR codes and resets after it.
type style struct {
	color color
	bold  bool
	dim   bool
}

func (s style) render(text string) string {
	var codes []string
	if s.bold {
		codes = append(codes, "1")
	}
	if s.dim {
		codes = append(codes, "2")
	}
	if code, colored := colorCodes[s.color]; colored {
		codes = append(codes, code)
	}
	if len(codes) == 0 || text == "" {
		return text
	}
	return "\x1b[" + strings.Join(codes, ";") + "m" + text + "\x1b[0m"
}

func paint(c color, text string) string { return style{color: c}.render(text) }
func bold(text string) string           { return style{bold: true}.render(text) }
func dim(text string) string            { return style{dim: true}.render(text) }

// width is the number of terminal cells the text takes, escape codes aside.
func width(text string) int { return ansi.StringWidth(text) }

// fit makes the text exactly n cells wide: cut with an ellipsis, or padded.
func fit(text string, n int) string {
	if n <= 0 {
		return ""
	}
	if width(text) > n {
		text = ansi.Truncate(text, n, "…")
	}
	return text + strings.Repeat(" ", n-width(text))
}

// sanitize makes text from outside (container output, key comments, error
// messages of other programs) safe to place on the screen: escape sequences
// and control characters are dropped, tabs become spaces.
func sanitize(text string) string {
	text = ansi.Strip(text)
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return -1
		}
		return r
	}, text)
}

// wrap breaks text into lines of at most n cells, at spaces where it can.
func wrap(text string, n int) []string {
	if n < 1 {
		n = 1
	}
	var lines []string
	for _, paragraph := range strings.Split(text, "\n") {
		line := ""
		for _, word := range strings.Fields(paragraph) {
			for width(word) > n {
				if line != "" {
					lines, line = append(lines, line), ""
				}
				head := ansi.Truncate(word, n, "")
				lines, word = append(lines, head), strings.TrimPrefix(word, head)
			}
			switch {
			case line == "":
				line = word
			case width(line)+1+width(word) <= n:
				line += " " + word
			default:
				lines, line = append(lines, line), word
			}
		}
		lines = append(lines, line)
	}
	return lines
}

// box draws a border around lines, making every line inner cells wide and
// the box rows tall in all (0: as tall as the content). Lines that do not
// fit are cut from the bottom.
func box(lines []string, inner, rows int) []string {
	if rows > 0 {
		body := rows - 2
		if body < 0 {
			body = 0
		}
		for len(lines) < body {
			lines = append(lines, "")
		}
		lines = lines[:body]
	}
	out := make([]string, 0, len(lines)+2)
	out = append(out, "┌"+strings.Repeat("─", inner+2)+"┐")
	for _, line := range lines {
		out = append(out, "│ "+fit(line, inner)+" │")
	}
	return append(out, "└"+strings.Repeat("─", inner+2)+"┘")
}

// overlay places the lines of top over base, with its left upper corner at
// column x, row y. What base shows to the left and right of top stays.
func overlay(base, top []string, x, y int) []string {
	out := append([]string{}, base...)
	for i, line := range top {
		row := y + i
		if row < 0 || row >= len(out) {
			continue
		}
		under := out[row]
		left := fit(ansi.Truncate(under, x, ""), x)
		right := ansi.TruncateLeft(under, x+width(line), "")
		out[row] = left + "\x1b[0m" + line + "\x1b[0m" + right
	}
	return out
}

// sideBySide joins two blocks of lines of the same height.
func sideBySide(left, right []string) []string {
	out := make([]string, 0, len(left))
	for i := range left {
		line := left[i]
		if i < len(right) {
			line += right[i]
		}
		out = append(out, line)
	}
	return out
}
