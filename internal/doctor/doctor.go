// Package doctor checks this machine's setup and the containers of its
// environments against what bh would create (internal/plan). The checks
// are functions from facts to findings: whoever calls them gathers the
// facts (cmd/bh for `bh doctor`) and decides how the findings are shown.
//
// The containers are checked as the engine reports them, not as bh
// remembers creating them: a container recreated by hand with the docker
// CLI, or one created by a bh with a bug, keeps the name and loses the
// boundary (threat model, "境界の前提条件").
package doctor

// Level is how a finding counts.
type Level string

const (
	OK   Level = "ok"
	Warn Level = "warn"
	// Fail: the boundary does not hold, or the setup cannot work as it is.
	// `bh doctor` exits with 1 when there is one.
	Fail Level = "fail"
	// Skip: the check did not apply (not created, a remote environment).
	Skip Level = "skip"
)

// Finding is the outcome of one check.
type Finding struct {
	Level   Level
	Message string
	// Standing marks a warning that an explicit setting asks for
	// (dockerSocket, publish). It is shown every time — the opt-in is
	// allowed, forgetting about it is not — but it is not news: Worst
	// leaves it out, so that it does not colour the environment for good.
	Standing bool
}

// Section is the findings about one thing: the machine, or an environment.
type Section struct {
	Title    string
	Findings []Finding
}

func ok(message string) Finding   { return Finding{Level: OK, Message: message} }
func warn(message string) Finding { return Finding{Level: Warn, Message: message} }
func fail(message string) Finding { return Finding{Level: Fail, Message: message} }
func skip(message string) Finding { return Finding{Level: Skip, Message: message} }

func standing(message string) Finding {
	return Finding{Level: Warn, Message: message, Standing: true}
}

// Failures counts the failed checks of all sections.
func Failures(sections []Section) int {
	count := 0
	for _, section := range sections {
		for _, finding := range section.Findings {
			if finding.Level == Fail {
				count++
			}
		}
	}
	return count
}

// Worst sums findings up as one level, for a mark next to a name: Fail if
// anything failed, Warn if something needs a look (standing warnings
// aside), Skip if nothing was checked, OK otherwise.
func Worst(findings []Finding) Level {
	worst := Skip
	for _, finding := range findings {
		switch {
		case finding.Level == Fail:
			return Fail
		case finding.Level == Warn && !finding.Standing:
			worst = Warn
		case finding.Level == OK && worst == Skip:
			worst = OK
		}
	}
	return worst
}
