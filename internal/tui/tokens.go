package tui

import (
	"fmt"

	"github.com/ys319/bulkhead/internal/doctor"
)

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
