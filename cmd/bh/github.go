package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/github"
)

const githubUsage = `usage: bh github <subcommand>

  github set <env> --preset <read|push|push+pr> [--owner <user|org>] [--days <n>] [--no-open]
                            issue a fine-grained token for <env>: opens GitHub's
                            token page filled in (pick the repositories there),
                            then reads the token (hidden; or from stdin)
  github status <env>       the token's preset, owner, repositories and expiry
  github revoke <env> [--yes]
                            revoke the token on GitHub and remove it from <env>

  presets: read = contents:read / push = contents:write /
           push+pr = contents:write + pull_requests:write (never workflows)
  The token can be read by everything inside the environment, Claude Code
  included — its scope is the only protection.`

// githubIssue is where a token is issued and the name it gets there.
type githubIssue struct {
	URL       string
	TokenName string
}

// githubRequest is the token to ask GitHub's page for.
type githubRequest struct {
	Preset string // one of config.GitHubPresets; there is no default
	Owner  string // a user or organization; empty for the user's own account
	Days   int    // until it expires; 0 for github.DefaultDays
}

// What an owner may look like: it goes into an address, and into a
// key=value line inside the environment.
var githubOwner = regexp.MustCompile(`^[!-~]{1,100}$`)

func (r *githubRequest) check() error {
	if r.Days == 0 {
		r.Days = github.DefaultDays
	}
	switch {
	case r.Preset == "":
		return usagef("choose the token's permissions with --preset %s (no default — push lets code leave the environment)",
			strings.Join(config.GitHubPresets, " | "))
	case !slices.Contains(config.GitHubPresets, r.Preset):
		return usagef("--preset takes %s (got: %s)", strings.Join(config.GitHubPresets, " / "), r.Preset)
	case r.Days < 1 || r.Days > github.MaxDays:
		return usagef("--days takes 1-%d (got: %d)", github.MaxDays, r.Days)
	case r.Owner != "" && !githubOwner.MatchString(r.Owner):
		return usagef("--owner takes the name of a GitHub user or organization")
	}
	return nil
}

// prepareGitHubIssue makes the address of GitHub's token page, filled in
// for the environment, and the name the token gets there. The name is
// passed on to setGitHubToken with the token the user brings back.
func (a *app) prepareGitHubIssue(envName string, request githubRequest) (githubIssue, error) {
	if err := request.check(); err != nil {
		return githubIssue{}, err
	}
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "this machine"
	}
	name := github.TokenName(envName, a.now())
	return githubIssue{
		TokenName: name,
		URL: github.TemplateURL(github.Template{
			Env: envName, Preset: request.Preset, Owner: request.Owner, Days: request.Days,
			TokenName: name, Hostname: hostname,
		}),
	}, nil
}

// githubSetResult is what setting a token came to.
type githubSetResult struct {
	Env   config.Environment
	State config.GitHubState
	// Replaced is the token this one took the place of, if another one.
	Replaced *config.GitHubState
	// RevokeErr: Replaced could not be revoked and is still valid at GitHub.
	RevokeErr error
	// SyncErr: the token is saved but not in the environment's volume; the
	// next bh up puts it there.
	SyncErr error
}

// setGitHubToken checks the token with GitHub, saves it, revokes the one it
// replaces and puts it into the environment.
//
// The old token is revoked only after the new one is verified and saved,
// and before the volume is written: writing may take long or be
// interrupted (the first time it builds an image), and by then the old
// token is gone from the record — it would stay valid with nothing left
// that points at it.
func (a *app) setGitHubToken(ctx context.Context, ref, token string, request githubRequest, tokenName string) (githubSetResult, error) {
	store, err := a.store()
	if err != nil {
		return githubSetResult{}, err
	}
	env, err := tokenHolder(store, ref)
	if err != nil {
		return githubSetResult{}, err
	}
	result := githubSetResult{Env: env}
	if err := request.check(); err != nil {
		return result, err
	}
	// The form is checked before anything is sent: whatever was pasted by
	// mistake does not go out to GitHub.
	token = strings.TrimSpace(token)
	if !config.ValidGitHubToken(token) {
		return result, errors.New("that is not a fine-grained personal access token (expected github_pat_...)")
	}
	previous, err := store.LoadGitHub(env.ID)
	if err != nil {
		return result, err
	}
	client := github.Client{HTTP: a.httpClient}
	info, err := client.Inspect(ctx, token)
	if err != nil {
		return result, err
	}

	now := a.now()
	// Without GitHub's word on the expiry, the day of issue plus the days
	// asked for stands in. An organization's limit may have cut it shorter.
	expiry := info.ExpiresAt
	if expiry.IsZero() {
		expiry = now.Add(time.Duration(request.Days) * 24 * time.Hour)
	}
	result.State = config.GitHubState{
		Token: token, Preset: request.Preset, Owner: request.Owner, TokenName: tokenName,
		IssuedAt: config.Timestamp(now), ExpiresAt: ptr(config.Timestamp(expiry)), Repos: info.Repos,
	}
	if result.State.Owner == "" {
		result.State.Owner = info.Login
	}
	if err := store.SaveGitHub(env.ID, result.State); err != nil {
		return result, err
	}
	// The volume is about to hold this token; nothing is left to clear.
	if err := store.SetGitHubClearPending(env.ID, false); err != nil {
		return result, err
	}
	if previous != nil && previous.Token != token {
		result.Replaced = previous
		result.RevokeErr = client.Revoke(ctx, previous.Token)
	}
	result.SyncErr = a.applyTokens(ctx, store, env.ID, volumeWriter.syncGitHub)
	return result, nil
}

// report words what setting the token came to: notes on what else was done,
// and warnings about what is left to do.
func (r githubSetResult) report() (notes, warnings []string) {
	if r.Replaced != nil {
		if r.RevokeErr == nil {
			notes = append(notes, fmt.Sprintf("revoked the previous token (%s)", r.Replaced.TokenName))
		} else {
			warnings = append(warnings, fmt.Sprintf("revoking the previous token failed (%v) — %s",
				r.RevokeErr, github.ManualRevokeHint(r.Replaced.TokenName)))
		}
	}
	if r.SyncErr != nil {
		warnings = append(warnings, fmt.Sprintf("the token is saved but not yet in the environment (%s) — bh up %s applies it",
			oneLine(r.SyncErr), r.Env.Name))
	}
	return notes, warnings
}

// githubToken is the environment's GitHub token as recorded on this
// machine; nil when it has none.
func (a *app) githubToken(ref string) (config.Environment, *config.GitHubState, error) {
	store, err := a.store()
	if err != nil {
		return config.Environment{}, nil, err
	}
	env, err := tokenHolder(store, ref)
	if err != nil {
		return env, nil, err
	}
	state, err := store.LoadGitHub(env.ID)
	return env, state, err
}

// githubRevokeResult is what revoking a token came to.
type githubRevokeResult struct {
	Env   config.Environment
	State config.GitHubState
	// ClearErr: the token is revoked but still in the environment's volume;
	// the next bh up takes it out.
	ClearErr error
}

// revokeGitHubToken revokes the environment's token at GitHub, forgets it
// and takes it out of the environment. If GitHub does not confirm, nothing
// is removed: the copy on this machine is what a retry needs.
func (a *app) revokeGitHubToken(ctx context.Context, ref string) (githubRevokeResult, error) {
	store, err := a.store()
	if err != nil {
		return githubRevokeResult{}, err
	}
	env, err := tokenHolder(store, ref)
	if err != nil {
		return githubRevokeResult{}, err
	}
	result := githubRevokeResult{Env: env}
	state, err := store.LoadGitHub(env.ID)
	if err != nil {
		return result, err
	}
	if state == nil {
		return result, fmt.Errorf("%q has no GitHub token", env.Name)
	}
	result.State = *state
	if err := (github.Client{HTTP: a.httpClient}).Revoke(ctx, state.Token); err != nil {
		return result, fmt.Errorf("revoking the token failed (%v) — nothing was removed; retry, or %s",
			err, github.ManualRevokeHint(state.TokenName))
	}
	if err := store.RemoveGitHub(env.ID); err != nil {
		return result, fmt.Errorf("the token is revoked, but its record could not be removed: %w", err)
	}
	result.ClearErr = a.applyTokens(ctx, store, env.ID, func(w volumeWriter, ctx context.Context, _ config.Store) error {
		return w.clearGitHub(ctx)
	})
	if result.ClearErr != nil {
		// A revoked token left in the volume keeps being handed out; the
		// mark makes the next bh up take it out.
		if err := store.SetGitHubClearPending(env.ID, true); err != nil {
			return result, fmt.Errorf("the token is revoked but still in the environment's volume (%s), and that could not be noted for the next start: %w",
				oneLine(result.ClearErr), err)
		}
	}
	return result, nil
}

// revokeBeforeDestroy revokes the token of an environment that is being
// destroyed, while bh still has its copy. It never stops the destruction:
// what is returned is a warning that says where to revoke by hand.
func (a *app) revokeBeforeDestroy(ctx context.Context, store config.Store, id string) string {
	state, err := store.LoadGitHub(id)
	if err != nil {
		return "the GitHub token's record could not be read, so the token was not revoked — check " + github.SettingsURL
	}
	if state == nil {
		return ""
	}
	if err := (github.Client{HTTP: a.httpClient}).Revoke(ctx, state.Token); err != nil {
		return fmt.Sprintf("revoking the GitHub token failed (%v) — %s", err, github.ManualRevokeHint(state.TokenName))
	}
	return ""
}

// githubSummary is the token in one line: "push · octo · 2 repos · expires in 12d".
func githubSummary(state config.GitHubState, now time.Time) string {
	repos := "repos unknown"
	if state.Repos != nil {
		repos = strconv.Itoa(len(state.Repos)) + " repos"
		if len(state.Repos) == 1 {
			repos = "1 repo"
		}
	}
	expiry := "no expiry"
	if left, expires := state.DaysLeft(now); expires {
		expiry = fmt.Sprintf("expires in %dd", left)
		if left < 0 {
			expiry = "EXPIRED on " + githubDate(*state.ExpiresAt)
		}
	}
	return strings.Join([]string{state.Preset, state.Owner, repos, expiry}, " · ")
}

// githubDate is the day of a recorded time.
func githubDate(timestamp string) string {
	if len(timestamp) > 10 {
		return timestamp[:10]
	}
	return timestamp
}

// maskToken shows which token it is without showing the token.
func maskToken(token string) string {
	if len(token) < 4 {
		return "github_pat_..."
	}
	return "github_pat_..." + token[len(token)-4:]
}

// A token that reaches no repository still reads public ones: GitHub lets
// every fine-grained token do that.
const noGrantedRepos = "(none — only public read access)"

func githubRepoList(repos []string) string {
	if len(repos) == 0 {
		return noGrantedRepos
	}
	return strings.Join(repos, ", ")
}

func (a *app) github(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("%s", githubUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "set":
		return a.githubSet(ctx, rest)

	case "status":
		if len(rest) != 1 {
			return usagef("usage: bh github status <env>")
		}
		env, state, err := a.githubToken(rest[0])
		if err != nil {
			return err
		}
		if state == nil {
			fmt.Fprintf(a.stdout, "%q has no GitHub token (bh github set %s --preset %s)\n",
				env.Name, env.Name, strings.Join(config.GitHubPresets, "|"))
			return nil
		}
		repos := "unknown (GitHub did not list them)"
		if state.Repos != nil {
			repos = githubRepoList(state.Repos)
		}
		expires := "never"
		if state.ExpiresAt != nil {
			expires = githubDate(*state.ExpiresAt)
		}
		fmt.Fprintf(a.stdout, "github: %s\n", githubSummary(*state, a.now()))
		fmt.Fprintf(a.stdout, "  token:   %s  (%s)\n", maskToken(state.Token), state.TokenName)
		fmt.Fprintf(a.stdout, "  repos:   %s\n", repos)
		fmt.Fprintf(a.stdout, "  issued:  %s\n", githubDate(state.IssuedAt))
		fmt.Fprintf(a.stdout, "  expires: %s\n", expires)
		return nil

	case "revoke":
		flags := flag.NewFlagSet("github revoke", flag.ContinueOnError)
		yes := flags.Bool("yes", false, "")
		words, err := parseFlags(flags, rest)
		if err != nil {
			return err
		}
		if len(words) != 1 {
			return usagef("usage: bh github revoke <env> [--yes]")
		}
		env, state, err := a.githubToken(words[0])
		if err != nil {
			return err
		}
		if state == nil {
			return fmt.Errorf("%q has no GitHub token", env.Name)
		}
		if !*yes {
			if !a.isTerminal() {
				return errors.New("revoke requires --yes when not interactive")
			}
			question := fmt.Sprintf("Revoke %s (%s) on GitHub and remove it from %q?", maskToken(state.Token), state.TokenName, env.Name)
			if !a.confirm(question) {
				return errAborted
			}
		}
		result, err := a.revokeGitHubToken(ctx, env.Name)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Revoked the GitHub token of %q (%s)\n", env.Name, result.State.TokenName)
		if result.ClearErr != nil {
			a.warn([]string{fmt.Sprintf("the revoked token is still in the environment's volume (%s) — bh up %s clears it",
				oneLine(result.ClearErr), env.Name)})
		}
		return nil
	}
	return usagef("%s", githubUsage)
}

func (a *app) githubSet(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("github set", flag.ContinueOnError)
	preset := flags.String("preset", "", "")
	owner := flags.String("owner", "", "")
	days := flags.String("days", "", "")
	noOpen := flags.Bool("no-open", false, "")
	words, err := parseFlags(flags, args)
	if err != nil {
		return err
	}
	if len(words) != 1 {
		return usagef("usage: bh github set <env> --preset <read|push|push+pr> [--owner <user|org>] [--days <n>] [--no-open]")
	}
	request := githubRequest{Preset: *preset, Owner: *owner}
	if *days != "" {
		// Digits only: strconv alone would take "+30" too.
		if strings.Trim(*days, "0123456789") != "" {
			return usagef("--days takes 1-%d (got: %s)", github.MaxDays, *days)
		}
		if request.Days, err = strconv.Atoi(*days); err != nil || request.Days == 0 {
			return usagef("--days takes 1-%d (got: %s)", github.MaxDays, *days)
		}
	}

	env, previous, err := a.githubToken(words[0])
	if err != nil {
		return err
	}
	issue, err := a.prepareGitHubIssue(env.Name, request)
	if err != nil {
		return err
	}
	out := a.stdout
	fmt.Fprintln(out, `1. On GitHub, choose "Only select repositories" and pick the ones`)
	fmt.Fprintf(out, "   %q needs, then generate the token:\n", env.Name)
	fmt.Fprintf(out, "   %s\n", issue.URL)
	fmt.Fprintln(out, "2. Paste the token below.")
	if previous != nil {
		fmt.Fprintf(out, "   (replaces %s — revoked once the new one is verified)\n", maskToken(previous.Token))
	}
	if !*noOpen {
		// Without a browser the address above is what the user goes by.
		_ = a.openURL(issue.URL)
	}
	token, err := a.readToken()
	if err != nil {
		return err
	}

	result, err := a.setGitHubToken(ctx, env.Name, token, request, issue.TokenName)
	if err != nil {
		return err
	}
	notes, warnings := result.report()
	fmt.Fprintf(out, "Set the GitHub token for %q:\n", env.Name)
	fmt.Fprintf(out, "  %s\n", githubSummary(result.State, a.now()))
	if result.State.Repos != nil {
		fmt.Fprintf(out, "  repos: %s\n", githubRepoList(result.State.Repos))
	}
	for _, note := range notes {
		fmt.Fprintf(out, "  %s\n", note)
	}
	a.warn(warnings)
	return nil
}
