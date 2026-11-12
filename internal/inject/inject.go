// Package inject is what bh puts into the volumes that carry credentials
// into an environment, in the form bh-guest (containers/dev/bh-guest) reads
// there. Data flows one way: bh writes on this machine, dev mounts the
// volumes read-only.
//
// The layout — file names, owner, modes — is the TypeScript version's, kept
// as it is: a migrated environment brings its volumes along untouched, and
// bh-guest did not change.
package inject

import (
	"strconv"
	"strings"

	"github.com/ys319/bulkhead/internal/config"
)

// Job is a shell script that changes one of the environment's volumes. It
// runs as root in a short-lived container with nothing but that volume.
type Job struct {
	Key    string // which volume: one of plan.InjectKeys
	Mount  string // where the script finds it
	Script string
}

// The scripts set owner and modes themselves (dev's user is uid 1000, as
// containers/dev/Dockerfile makes it), so that nothing depends on how the
// volume was created or on who runs bh.

// GitHubWrite replaces the GitHub token and its metadata with what it reads
// from its input: the token on the first line, the metadata after it.
//
// The order matters. umask comes before the first write, so that no file
// with the token exists with wider permissions even for a moment. meta is
// moved into place before token: bh-guest goes by whether token is there,
// and must never find a new token next to the old metadata.
var GitHubWrite = Job{Key: "github", Mount: "/g", Script: strings.Join([]string{
	"set -eu",
	"umask 077",
	"cat > /g/.incoming",
	"head -n 1 /g/.incoming > /g/.token.tmp",
	"tail -n +2 /g/.incoming > /g/.meta.tmp",
	"rm -f /g/.incoming",
	"chown 1000:1000 /g /g/.token.tmp /g/.meta.tmp",
	"chmod 700 /g",
	"chmod 600 /g/.token.tmp",
	"chmod 644 /g/.meta.tmp",
	"mv /g/.meta.tmp /g/meta",
	"mv /g/.token.tmp /g/token",
}, "\n")}

// GitHubClear takes the GitHub token out of the volume. It empties the
// volume; the volume itself stays (only bh destroy removes volumes).
var GitHubClear = Job{Key: "github", Mount: "/g", Script: "rm -f /g/token /g/meta"}

// TokensWrite replaces the file of the other tokens with its input. The
// directory is closed to others first, then the file is swapped in whole.
var TokensWrite = Job{Key: "tokens", Mount: "/t", Script: strings.Join([]string{
	"set -eu",
	"umask 077",
	"chown 1000:1000 /t",
	"chmod 700 /t",
	"cat > /t/.env.tmp",
	"chown 1000:1000 /t/.env.tmp",
	"chmod 600 /t/.env.tmp",
	"mv /t/.env.tmp /t/env",
}, "\n")}

// GitHubMeta is what the environment is told about its GitHub token:
// key=value lines, which bh-guest reads with sed. The token is not in it.
func GitHubMeta(state config.GitHubState) string {
	lines := []string{
		"preset=" + state.Preset,
		"owner=" + state.Owner,
		"repos=" + strings.Join(state.Repos, ","),
	}
	if expiry, expires := state.Expiry(); expires {
		lines = append(lines,
			"expires_epoch="+strconv.FormatInt(expiry.Unix(), 10),
			"expires_date="+expiry.UTC().Format("2006-01-02"))
	}
	return strings.Join(lines, "\n") + "\n"
}

// GitHubInput is the input of GitHubWrite.
func GitHubInput(state config.GitHubState) []byte {
	return []byte(state.Token + "\n" + GitHubMeta(state))
}

// TokensInput is the input of TokensWrite: one NAME=value line per token.
// Kind and date stay on this machine.
func TokensInput(tokens []config.TokenEntry) []byte {
	var text strings.Builder
	for _, token := range tokens {
		text.WriteString(token.Name + "=" + token.Value + "\n")
	}
	return []byte(text.String())
}
