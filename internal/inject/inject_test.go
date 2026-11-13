package inject

import (
	"slices"
	"strings"
	"testing"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/plan"
)

const token = "github_pat_11AAAAAAA0aaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func sample() config.Token {
	expires := "2026-11-11T06:30:00.000Z"
	return config.Token{
		Service: config.ServiceGitHub, Preset: "push", Owner: "octo", TokenName: "bh-main-10121530",
		IssuedAt: "2026-10-12T06:30:00.000Z", ExpiresAt: &expires, Repos: []string{"octo/foo", "octo/bar"},
	}
}

// What bh-guest reads with sed, key by key. 1794378600 is 2026-11-11T06:30Z.
func TestGitHubMeta(t *testing.T) {
	meta := GitHubMeta(sample())
	want := "preset=push\nowner=octo\nrepos=octo/foo,octo/bar\nexpires_epoch=1794378600\nexpires_date=2026-11-11\n"
	if meta != want {
		t.Errorf("meta = %q, want %q", meta, want)
	}
	if strings.Contains(meta, "github_pat_") {
		t.Error("the metadata contains the token")
	}

	// Without an expiry the two lines are left out (bh-guest reads that as
	// "does not expire"); unknown repositories are an empty list.
	open := sample()
	open.ExpiresAt, open.Repos = nil, nil
	if meta := GitHubMeta(open); meta != "preset=push\nowner=octo\nrepos=\n" {
		t.Errorf("meta = %q", meta)
	}
}

func TestGitHubInputIsTheTokenLineThenTheMetadata(t *testing.T) {
	input := string(GitHubInput(sample(), token))
	first, rest, _ := strings.Cut(input, "\n")
	if first != token || rest != GitHubMeta(sample()) {
		t.Errorf("input = %q", input)
	}
}

func TestEnvInput(t *testing.T) {
	input := EnvInput([]Variable{
		{Name: "HF_TOKEN", Value: "hf_a"},
		{Name: "K", Value: "a=b"},
		{Name: "GIT_AUTHOR_NAME", Value: "It's Me"},
	})
	if string(input) != "HF_TOKEN=hf_a\nK=a=b\nGIT_AUTHOR_NAME=It's Me\n" {
		t.Errorf("input = %q", input)
	}
	// Nothing is an empty file, which is what takes the last one out.
	if input := EnvInput(nil); len(input) != 0 {
		t.Errorf("input = %#v", input)
	}
}

func TestJobsWorkOnTheVolumesDevMounts(t *testing.T) {
	for _, job := range []Job{GitHubWrite, GitHubClear, TokensWrite} {
		if !slices.Contains(plan.InjectKeys, job.Key) {
			t.Errorf("%q is not a volume that carries credentials", job.Key)
		}
		// Everything a script touches is inside its volume.
		for _, word := range strings.Fields(job.Script) {
			if strings.HasPrefix(word, "/") && word != job.Mount && !strings.HasPrefix(word, job.Mount+"/") {
				t.Errorf("%s: the script touches %s", job.Key, word)
			}
		}
		if strings.Contains(job.Script, "github_pat_") {
			t.Errorf("%s: a token in the script", job.Key)
		}
	}
}

// at is the line's position in the script; the line must be there.
func at(t *testing.T, script, line string) int {
	t.Helper()
	i := slices.Index(strings.Split(script, "\n"), line)
	if i < 0 {
		t.Fatalf("the script lacks %q:\n%s", line, script)
	}
	return i
}

func TestGitHubWriteScript(t *testing.T) {
	script := GitHubWrite.Script
	lines := strings.Split(script, "\n")
	// No file with the token exists with wider permissions, even for a moment.
	firstWrite := slices.IndexFunc(lines, func(line string) bool { return strings.Contains(line, ">") })
	if firstWrite < 0 || at(t, script, "umask 077") > firstWrite || at(t, script, "set -eu") != 0 {
		t.Errorf("umask does not come before the first write:\n%s", script)
	}
	at(t, script, "chown 1000:1000 /g /g/.token.tmp /g/.meta.tmp")
	at(t, script, "chmod 700 /g")
	at(t, script, "chmod 600 /g/.token.tmp")
	at(t, script, "chmod 644 /g/.meta.tmp")
	// A new token never stands next to the old metadata: bh-guest goes by
	// whether the token is there.
	if at(t, script, "mv /g/.meta.tmp /g/meta") > at(t, script, "mv /g/.token.tmp /g/token") {
		t.Errorf("the token is moved into place before its metadata:\n%s", script)
	}
	// Owner and modes are settled before anything is moved into place.
	if at(t, script, "chmod 600 /g/.token.tmp") > at(t, script, "mv /g/.token.tmp /g/token") {
		t.Errorf("the token is moved into place before its mode is set:\n%s", script)
	}
	if at(t, script, "rm -f /g/.incoming") > at(t, script, "mv /g/.meta.tmp /g/meta") {
		t.Errorf("the input is still there when the files are in place:\n%s", script)
	}
}

func TestTokensWriteScript(t *testing.T) {
	script := TokensWrite.Script
	if at(t, script, "umask 077") > at(t, script, "cat > /t/.env.tmp") {
		t.Errorf("umask does not come before the write:\n%s", script)
	}
	// The directory is closed before the file is written into it.
	if at(t, script, "chmod 700 /t") > at(t, script, "cat > /t/.env.tmp") {
		t.Errorf("the file is written before the directory is closed:\n%s", script)
	}
	at(t, script, "chown 1000:1000 /t")
	at(t, script, "chown 1000:1000 /t/.env.tmp")
	at(t, script, "chmod 600 /t/.env.tmp")
	if !strings.HasSuffix(script, "\nmv /t/.env.tmp /t/env") {
		t.Errorf("the file is not swapped in last:\n%s", script)
	}
}

func TestGitHubClearOnlyRemovesTheTwoFiles(t *testing.T) {
	if GitHubClear.Script != "rm -f /g/token /g/meta" {
		t.Errorf("script = %q", GitHubClear.Script)
	}
}
