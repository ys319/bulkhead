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
	"github.com/ys319/bulkhead/internal/huggingface"
	"github.com/ys319/bulkhead/internal/tui"
	"github.com/ys319/bulkhead/internal/vault"
)

const tokenUsage = `usage: bh token <subcommand>

  token list [<env>]        the tokens of <env> (or of every environment):
                            service, access, owner, expiry
  token set [<env>] [<service>] [<preset>] [--owner <user|org>] [--days <n>] [--no-open]
                            get a token from the service for <env>: opens its
                            page (filled in where the service allows), then
                            reads the token (hidden; or from stdin). A token
                            the environment already has from that service is
                            replaced, and revoked
  token refresh [<env>] [<service>] [--no-open]
                            the same token again with a new value and expiry:
                            regenerate it on the service's page, paste it here
  token remove [<env>] [<service>] [--yes]
                            take the token out of <env> and revoke it

  At a terminal, what is left out is asked for, in that order.

  services and presets:
    github        read        clone and fetch private repositories
                  push        also push, and read the results of CI
                  contribute  also open, update and review pull requests
                  (never workflows; pick the repositories on GitHub's page)
    huggingface   no presets: choose the token's type on its page (hf for short)

  A token is a credential the service issues for this environment; bh knows
  how to check, renew and revoke it. For a secret value of your own use
  bh secret, for a plain variable bh var. A token can be read by everything
  inside the environment, Claude Code included — what it may do is the only
  protection.`

// tokenPreset is a set of permissions a token is asked for, named by the
// furthest thing code inside the environment becomes able to do with it.
type tokenPreset struct{ Name, About string }

// tokenService is a service bh can get a token from.
type tokenService struct {
	Name  string // what is typed and recorded
	Alias string // a shorter word for it
	Title string // what it is called in sentences
	// Presets are the permission sets to choose from, each including the
	// one before. A service has none when its own page asks well enough
	// and cannot be opened filled in.
	Presets []tokenPreset
	// Variable is the name the token comes under inside the environment.
	Variable string
}

var tokenServices = []tokenService{
	{Name: config.ServiceGitHub, Title: "GitHub", Variable: "GH_TOKEN", Presets: []tokenPreset{
		{"read", "clone and fetch private repositories"},
		{"push", "also push — rewriting unprotected branches included — and read the results of CI"},
		{"contribute", "also open, update and review pull requests"},
	}},
	{Name: config.ServiceHuggingFace, Alias: "hf", Title: "Hugging Face", Variable: huggingface.Variable},
}

func serviceNamed(word string) (tokenService, bool) {
	for _, service := range tokenServices {
		if word == service.Name || (service.Alias != "" && word == service.Alias) {
			return service, true
		}
	}
	return tokenService{}, false
}

func serviceNames() string {
	names := make([]string, len(tokenServices))
	for i, service := range tokenServices {
		names[i] = service.Name
	}
	return strings.Join(names, " | ")
}

func (s tokenService) presetNames() []string {
	names := make([]string, len(s.Presets))
	for i, preset := range s.Presets {
		names[i] = preset.Name
	}
	return names
}

// choose asks for an argument that was left out: at a terminal, from the
// choices; anywhere else that is a usage error which names what is missing.
func (a *app) choose(ctx context.Context, usage, missing, title string, choices []tui.Choice) (string, error) {
	if !a.isTerminal() {
		return "", usagef("usage: bh %s\n(%s is missing; at a terminal it is asked for)", usage, missing)
	}
	if len(choices) == 0 {
		return "", usagef("usage: bh %s\n(there is nothing to choose %s from)", usage, missing)
	}
	picked, err := a.pick(ctx, title, choices)
	if errors.Is(err, tui.ErrNothingPicked) {
		return "", exitCode(1)
	}
	return picked, err
}

// tokenSummary is a token in one line: "push · octo · 2 repos · expires in 12d".
func tokenSummary(token config.Token, now time.Time) string {
	var parts []string
	for _, part := range []string{token.Preset, token.Access, token.Owner} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	if token.Service == config.ServiceGitHub {
		repos := "repos unknown"
		if token.Repos != nil {
			repos = strconv.Itoa(len(token.Repos)) + " repos"
			if len(token.Repos) == 1 {
				repos = "1 repo"
			}
		}
		parts = append(parts, repos)
	}
	expiry := "no expiry"
	if left, expires := token.DaysLeft(now); expires {
		expiry = fmt.Sprintf("expires in %dd", left)
		if left < 0 {
			expiry = "EXPIRED on " + day(*token.ExpiresAt)
		}
	}
	return strings.Join(append(parts, expiry), " · ")
}

// day is the day of a recorded time.
func day(timestamp string) string {
	if len(timestamp) > 10 {
		return timestamp[:10]
	}
	return timestamp
}

// A token that reaches no repository still reads public ones: GitHub lets
// every fine-grained token do that.
const noGrantedRepos = "(none — only public read access)"

func repoList(repos []string) string {
	if repos == nil {
		return "unknown (GitHub did not list them)"
	}
	if len(repos) == 0 {
		return noGrantedRepos
	}
	return strings.Join(repos, ", ")
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
	case !slices.Contains(config.GitHubPresets, r.Preset):
		return usagef("github takes the preset %s (got: %q) — there is no default: push lets code leave the environment",
			strings.Join(config.GitHubPresets, " | "), r.Preset)
	case r.Days < 1 || r.Days > github.MaxDays:
		return usagef("--days takes 1-%d (got: %d)", github.MaxDays, r.Days)
	case r.Owner != "" && !githubOwner.MatchString(r.Owner):
		return usagef("--owner takes the name of a GitHub user or organization")
	}
	return nil
}

// tokenIssue is where a token is made, what to do there, and — for a
// service whose page bh fills in — the name the token gets.
type tokenIssue struct {
	URL       string
	Steps     []string
	TokenName string
}

// issueFor prepares getting a new token from the service for the
// environment.
func (a *app) issueFor(service tokenService, envName string, request githubRequest) (tokenIssue, error) {
	switch service.Name {
	case config.ServiceGitHub:
		if err := request.check(); err != nil {
			return tokenIssue{}, err
		}
		hostname, err := os.Hostname()
		if err != nil || hostname == "" {
			hostname = "this machine"
		}
		name := github.TokenName(envName, a.now())
		address := github.TemplateURL(github.Template{
			Env: envName, Preset: request.Preset, Owner: request.Owner, Days: request.Days,
			TokenName: name, Hostname: hostname,
		})
		return tokenIssue{URL: address, TokenName: name, Steps: []string{
			fmt.Sprintf(`On GitHub, choose "Only select repositories" and pick the ones %q needs, then generate the token:`, envName),
		}}, nil
	case config.ServiceHuggingFace:
		return tokenIssue{URL: huggingface.TokensURL, Steps: []string{
			fmt.Sprintf("On Hugging Face, make a token for %q — its type is your choice there (read is enough to download models):", envName),
		}}, nil
	}
	return tokenIssue{}, fmt.Errorf("%q is not a service bh gets tokens from (%s)", service.Name, serviceNames())
}

// reissueFor prepares getting the same token again with a new value: the
// service's own way of regenerating it, which keeps what it may do.
func reissueFor(service tokenService, token config.Token) tokenIssue {
	switch service.Name {
	case config.ServiceGitHub:
		return tokenIssue{URL: github.SettingsURL, TokenName: token.TokenName, Steps: []string{
			fmt.Sprintf(`On GitHub, open the token %q and choose "Regenerate token" — its repositories and permissions stay:`, token.TokenName),
		}}
	default:
		return tokenIssue{URL: huggingface.TokensURL, Steps: []string{
			`On Hugging Face, open the token's "Manage" menu and choose "Invalidate and refresh":`,
		}}
	}
}

// tokenSet is what giving an environment a token came to.
type tokenSet struct {
	Env     config.Environment
	Service tokenService
	Token   config.Token
	// Replaced is the token this one took the place of, if it had another
	// value.
	Replaced *config.Token
	// RevokeErr: the replaced token could not be revoked and may still be
	// valid at the service.
	RevokeErr error
	// Notes are what the service's answer gives to mention.
	Notes []string
	// SyncErr: the token is saved but not in the environment's volume; the
	// next bh up puts it there.
	SyncErr error
}

// setToken checks the pasted value with the service, records the token,
// revokes the one it replaces and puts it into the environment.
//
// The old token is revoked only after the new one is verified and saved,
// and before the volume is written: writing may take long or be
// interrupted (the first time it builds an image), and by then the old
// value is gone from this machine — it would stay valid with nothing left
// that points at it.
//
// refresh: the token is the one the environment has, regenerated. What bh
// asked for it stays as recorded; what the service says now is compared
// with what it said before.
func (a *app) setToken(ctx context.Context, ref string, service tokenService, value string, request githubRequest, tokenName string, refresh bool) (tokenSet, error) {
	store, err := a.store()
	if err != nil {
		return tokenSet{}, err
	}
	env, err := handedTo(store, ref)
	if err != nil {
		return tokenSet{}, err
	}
	result := tokenSet{Env: env, Service: service}
	// The form is checked before anything is sent: whatever was pasted by
	// mistake does not go out to the service.
	value = strings.TrimSpace(value)
	creds, err := a.credentialsOf(store, env, true)
	if err != nil {
		return result, err
	}
	previous := creds.token(service.Name)
	var previousValue string
	if previous != nil {
		if previousValue, err = creds.vault.Get(config.TokenKey(service.Name)); err != nil && !errors.Is(err, vault.ErrNotFound) {
			return result, fmt.Errorf("the %s token %q has now: %w", service.Title, env.Name, err)
		}
	}
	if refresh {
		if previous == nil {
			return result, fmt.Errorf("%q has no %s token to refresh (bh token set %s %s)", env.Name, service.Title, env.Name, service.Name)
		}
		if value == previousValue {
			return result, fmt.Errorf("that is the token %q already has — regenerate it on the page first", env.Name)
		}
	}
	now := a.now()
	token := config.Token{Service: service.Name, TokenName: tokenName, IssuedAt: config.Timestamp(now)}

	switch service.Name {
	case config.ServiceGitHub:
		if err := request.check(); err != nil {
			return result, err
		}
		if !config.ValidGitHubToken(value) {
			return result, errors.New("that is not a fine-grained personal access token (expected github_pat_...)")
		}
		info, err := (github.Client{HTTP: a.httpClient}).Inspect(ctx, value)
		if err != nil {
			return result, err
		}
		// Without GitHub's word on the expiry, the day of issue plus the
		// days asked for stands in. An organization's limit may have cut
		// it shorter.
		expiry := info.ExpiresAt
		if expiry.IsZero() {
			expiry = now.Add(time.Duration(request.Days) * 24 * time.Hour)
		}
		token.Preset, token.Owner, token.Repos = request.Preset, request.Owner, info.Repos
		token.ExpiresAt = ptr(config.Timestamp(expiry))
		if token.Owner == "" {
			token.Owner = info.Login
		}
		if refresh && previous.Repos != nil && info.Repos != nil && !slices.Equal(sorted(previous.Repos), sorted(info.Repos)) {
			result.Notes = append(result.Notes, fmt.Sprintf("its repositories are not the ones it had: now %s (before: %s)", repoList(info.Repos), repoList(previous.Repos)))
		}

	case config.ServiceHuggingFace:
		if !huggingface.ValidToken(value) {
			return result, errors.New("that is not a Hugging Face access token (expected hf_...)")
		}
		info, err := (huggingface.Client{HTTP: a.httpClient}).Inspect(ctx, value)
		switch {
		case errors.Is(err, huggingface.ErrRejected):
			return result, err
		case err != nil:
			// The question failing says nothing about the token. Hugging
			// Face has no expiry or scope bh depends on, so the token is
			// taken as given.
			result.Notes = append(result.Notes, fmt.Sprintf("Hugging Face could not be asked about it (%s) — taken as given", oneLine(err)))
		default:
			token.Access, token.Owner = info.Role, info.Account
			if token.TokenName == "" {
				token.TokenName = info.TokenName
			}
		}
		if refresh && previous.Access != "" && token.Access != "" && previous.Access != token.Access {
			result.Notes = append(result.Notes, fmt.Sprintf("its type is %s now (before: %s)", token.Access, previous.Access))
		}

	default:
		return result, fmt.Errorf("%q is not a service bh gets tokens from (%s)", service.Name, serviceNames())
	}

	if err := creds.putToken(token, value); err != nil {
		return result, err
	}
	result.Token = token
	if previous != nil && previousValue != "" && previousValue != value {
		result.Replaced = previous
		result.RevokeErr = a.revokeAtService(ctx, service.Name, previousValue)
	}
	if service.Name == config.ServiceGitHub {
		result.SyncErr = a.applyGitHub(ctx, creds)
	} else {
		result.SyncErr = a.applyEnv(ctx, creds)
	}
	return result, nil
}

func sorted(list []string) []string {
	out := slices.Clone(list)
	slices.Sort(out)
	return out
}

// revokeAtService makes the service invalidate the token's value. Both
// services take the value as its own proof.
func (a *app) revokeAtService(ctx context.Context, service, value string) error {
	switch service {
	case config.ServiceGitHub:
		return (github.Client{HTTP: a.httpClient}).Revoke(ctx, value)
	case config.ServiceHuggingFace:
		return (huggingface.Client{HTTP: a.httpClient}).Revoke(ctx, value)
	}
	return fmt.Errorf("bh cannot revoke a %s token", service)
}

// revokeHint tells where to revoke a token that bh could not revoke.
func revokeHint(token config.Token) string {
	switch token.Service {
	case config.ServiceGitHub:
		return github.ManualRevokeHint(token.TokenName)
	case config.ServiceHuggingFace:
		return "delete it by hand at " + huggingface.TokensURL
	}
	return "revoke it at the service by hand"
}

// report words what setting the token came to: notes on what else there is
// to say, and warnings about what is left to do.
func (r tokenSet) report() (notes, warnings []string) {
	notes = append(notes, r.Notes...)
	if r.Replaced != nil {
		if r.RevokeErr == nil {
			notes = append(notes, "revoked the previous value")
		} else {
			warnings = append(warnings, fmt.Sprintf("revoking the previous value failed (%v) — %s", r.RevokeErr, revokeHint(*r.Replaced)))
		}
	}
	if r.SyncErr != nil {
		warnings = append(warnings, notYet(r.Env, r.SyncErr))
	}
	return notes, warnings
}

// tokenRemoved is what removing a token came to.
type tokenRemoved struct {
	Env     config.Environment
	Service tokenService
	Token   config.Token
	// SyncErr: the token is revoked and forgotten but still in the
	// environment's volume; the next bh up takes it out.
	SyncErr error
}

// removeToken revokes the environment's token at the service, forgets it
// and takes it out of the environment. If the service does not confirm,
// nothing is removed: the copy on this machine is what a retry needs.
func (a *app) removeToken(ctx context.Context, ref string, service tokenService) (tokenRemoved, error) {
	store, err := a.store()
	if err != nil {
		return tokenRemoved{}, err
	}
	env, err := holderOf(store, ref)
	if err != nil {
		return tokenRemoved{}, err
	}
	result := tokenRemoved{Env: env, Service: service}
	creds, err := a.credentialsOf(store, env, false)
	if err != nil {
		return result, err
	}
	token := creds.token(service.Name)
	if token == nil {
		return result, fmt.Errorf("%q has no %s token", env.Name, service.Title)
	}
	result.Token = *token
	value, err := creds.vault.Get(config.TokenKey(service.Name))
	switch {
	case errors.Is(err, vault.ErrNotFound):
		// The record without its value: there is nothing to revoke with.
		a.warn([]string{fmt.Sprintf("the value of the %s token is not where it should be kept, so it was not revoked — %s", service.Title, revokeHint(*token))})
	case err != nil:
		return result, fmt.Errorf("the %s token: %w", service.Title, err)
	default:
		if err := a.revokeAtService(ctx, service.Name, value); err != nil {
			return result, fmt.Errorf("revoking the token failed (%v) — nothing was removed; retry, or %s", err, revokeHint(*token))
		}
	}
	if err := creds.dropToken(service.Name); err != nil {
		return result, fmt.Errorf("the token is revoked, but its record could not be removed: %w", err)
	}
	// An archived environment has no volumes to take it out of.
	if service.Name == config.ServiceGitHub {
		result.SyncErr = a.applyGitHub(ctx, creds)
	} else {
		result.SyncErr = a.applyEnv(ctx, creds)
	}
	return result, nil
}

// revokeTokens revokes every token of an environment that is going away
// (destroyed) or to sleep (archived) and forgets them. It never stops what
// it is part of: what comes back are warnings that say where to revoke by
// hand. A token that could not be revoked keeps its record when keep is
// true — it is what a retry needs.
func (a *app) revokeTokens(ctx context.Context, creds *credentials, keep bool) (revoked []string, warnings []string) {
	for _, token := range slices.Clone(creds.Tokens) {
		service, _ := serviceNamed(token.Service)
		title := service.Title
		if title == "" {
			title = token.Service
		}
		value, err := creds.vault.Get(config.TokenKey(token.Service))
		if err == nil {
			err = a.revokeAtService(ctx, token.Service, value)
		}
		if err != nil {
			warning := fmt.Sprintf("revoking the %s token failed (%s) — it still works; ", title, oneLine(err))
			if keep {
				warning += fmt.Sprintf("bh token remove %s %s retries, or ", creds.env.Name, token.Service)
			}
			warnings = append(warnings, warning+revokeHint(token))
			if keep {
				continue
			}
		} else {
			revoked = append(revoked, title)
		}
		if err := creds.dropToken(token.Service); err != nil {
			warnings = append(warnings, fmt.Sprintf("the record of the %s token could not be removed: %v", title, err))
		}
	}
	return revoked, warnings
}

func (a *app) token(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("%s", tokenUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return a.tokenList(rest)
	case "set":
		return a.tokenSetCommand(ctx, rest, false)
	case "refresh":
		return a.tokenSetCommand(ctx, rest, true)
	case "remove":
		return a.tokenRemoveCommand(ctx, rest)
	}
	return usagef("%s", tokenUsage)
}

func (a *app) tokenList(args []string) error {
	if len(args) > 1 {
		return usagef("usage: bh token list [<env>]")
	}
	store, cfg, err := a.load()
	if err != nil {
		return err
	}
	envs := cfg.Envs
	if len(args) == 1 {
		env, err := holderOf(store, args[0])
		if err != nil {
			return err
		}
		envs = []config.Environment{env}
	}
	shown := 0
	var problems []string
	for _, env := range envs {
		if !env.Local() {
			continue
		}
		creds, err := a.credentialsOf(store, env, false)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", env.Name, err))
			continue
		}
		for _, token := range creds.Tokens {
			fmt.Fprintf(a.stdout, "%-16s %-12s %s\n", env.Name, token.Service, tokenSummary(token, a.now()))
			shown++
			if len(args) == 1 {
				// One environment asked about: the details too.
				if token.TokenName != "" {
					fmt.Fprintf(a.stdout, "  name:    %s\n", token.TokenName)
				}
				if token.Service == config.ServiceGitHub {
					fmt.Fprintf(a.stdout, "  repos:   %s\n", repoList(token.Repos))
				}
				fmt.Fprintf(a.stdout, "  issued:  %s\n", day(token.IssuedAt))
				expires := "never"
				if token.ExpiresAt != nil {
					expires = day(*token.ExpiresAt)
				}
				fmt.Fprintf(a.stdout, "  expires: %s\n", expires)
			}
			switch token.Urgency(a.now()) {
			case "expired":
				a.warn([]string{fmt.Sprintf("the %s token of %q has expired — bh token refresh %s %s", token.Service, env.Name, env.Name, token.Service)})
			case "soon":
				a.warn([]string{fmt.Sprintf("the %s token of %q expires soon — bh token refresh %s %s", token.Service, env.Name, env.Name, token.Service)})
			}
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	if shown == 0 {
		if len(args) == 1 {
			fmt.Fprintf(a.stdout, "%q has no tokens (bh token set %s)\n", envs[0].Name, envs[0].Name)
		} else {
			fmt.Fprintln(a.stdout, "No tokens (bh token set <env>)")
		}
	}
	return nil
}

// tokenEnvironment is the first step of every token command: which
// environment. Asked for when left out.
func (a *app) tokenEnvironment(ctx context.Context, usage string, words []string, having string) (config.Store, config.Environment, []string, error) {
	store, cfg, err := a.load()
	if err != nil {
		return store, config.Environment{}, nil, err
	}
	if len(words) == 0 {
		var choices []tui.Choice
		for _, env := range cfg.Envs {
			if env.Local() && env.Archived == "" {
				choices = append(choices, tui.Choice{Name: env.Name})
			}
		}
		name, err := a.choose(ctx, usage, "the environment", "Which environment?", choices)
		if err != nil {
			return store, config.Environment{}, nil, err
		}
		words = []string{name}
	}
	env, err := holderOf(store, words[0])
	return store, env, words[1:], err
}

func (a *app) tokenSetCommand(ctx context.Context, args []string, refresh bool) error {
	command, usage := "token set", "token set [<env>] [<service>] [<preset>] [--owner <user|org>] [--days <n>] [--no-open]"
	if refresh {
		command, usage = "token refresh", "token refresh [<env>] [<service>] [--no-open]"
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	owner := flags.String("owner", "", "")
	days := flags.String("days", "", "")
	noOpen := flags.Bool("no-open", false, "")
	words, err := parseFlags(flags, args)
	if err != nil {
		return err
	}
	limit := 3
	if refresh {
		limit = 2
	}
	if len(words) > limit || (refresh && (*owner != "" || *days != "")) {
		return usagef("usage: bh %s", usage)
	}
	request := githubRequest{Owner: *owner}
	if *days != "" {
		// Digits only: strconv alone would take "+30" too.
		if strings.Trim(*days, "0123456789") != "" {
			return usagef("--days takes 1-%d (got: %s)", github.MaxDays, *days)
		}
		if request.Days, err = strconv.Atoi(*days); err != nil || request.Days == 0 {
			return usagef("--days takes 1-%d (got: %s)", github.MaxDays, *days)
		}
	}

	store, env, words, err := a.tokenEnvironment(ctx, usage, words, "")
	if err != nil {
		return err
	}
	if env.Archived != "" {
		return archivedError(env)
	}
	creds, err := a.credentialsOf(store, env, false)
	if err != nil {
		return err
	}

	// The service.
	var service tokenService
	if len(words) == 0 {
		var choices []tui.Choice
		for _, candidate := range tokenServices {
			has := creds.token(candidate.Name) != nil
			switch {
			case refresh && !has:
				continue
			case has:
				choices = append(choices, tui.Choice{Name: candidate.Name, Note: candidate.Title + " — " + tokenSummary(*creds.token(candidate.Name), a.now())})
			default:
				choices = append(choices, tui.Choice{Name: candidate.Name, Note: candidate.Title})
			}
		}
		if refresh && len(choices) == 0 {
			return fmt.Errorf("%q has no tokens to refresh (bh token set %s)", env.Name, env.Name)
		}
		picked := ""
		if refresh && len(choices) == 1 {
			picked = choices[0].Name // the only one it has
		} else if picked, err = a.choose(ctx, usage, "the service", "Which service?", choices); err != nil {
			return err
		}
		words = []string{picked}
	}
	service, known := serviceNamed(words[0])
	if !known {
		return usagef("%q is not a service bh gets tokens from (%s)", words[0], serviceNames())
	}
	words = words[1:]
	previous := creds.token(service.Name)

	var issue tokenIssue
	tokenName := ""
	if refresh {
		if previous == nil {
			return fmt.Errorf("%q has no %s token to refresh (bh token set %s %s)", env.Name, service.Title, env.Name, service.Name)
		}
		issue = reissueFor(service, *previous)
		tokenName = previous.TokenName
		// What bh asked for stays as it was.
		request.Preset, request.Owner = previous.Preset, previous.Owner
	} else {
		// The preset, for a service that has them.
		asked := false
		switch {
		case len(service.Presets) == 0 && len(words) > 0:
			return usagef("%s has no presets: the token's type is chosen on its page (bh token set %s %s)", service.Name, env.Name, service.Name)
		case len(service.Presets) == 0:
		case len(words) == 0:
			var choices []tui.Choice
			for _, preset := range service.Presets {
				choices = append(choices, tui.Choice{Name: preset.Name, Note: preset.About})
			}
			if request.Preset, err = a.choose(ctx, usage, "the preset ("+strings.Join(service.presetNames(), " | ")+")",
				"What may code in "+env.Name+" do on "+service.Title+"?", choices); err != nil {
				return err
			}
			asked = true
		default:
			request.Preset = words[0]
		}
		// The owner is asked for only in a conversation that is already
		// going on; given as arguments, the command is taken at its word.
		if asked && service.Name == config.ServiceGitHub && *owner == "" {
			fmt.Fprint(a.stderr, "Owner of the repositories — a user or an organization (Enter for your own account): ")
			line, _ := readLine(a.stdin)
			request.Owner = strings.TrimSpace(line)
		}
		if issue, err = a.issueFor(service, env.Name, request); err != nil {
			return err
		}
		tokenName = issue.TokenName
	}

	out := a.stdout
	for i, step := range issue.Steps {
		fmt.Fprintf(out, "%d. %s\n", i+1, step)
	}
	fmt.Fprintf(out, "   %s\n", issue.URL)
	fmt.Fprintf(out, "%d. Paste the token below.\n", len(issue.Steps)+1)
	if previous != nil && !refresh {
		fmt.Fprintf(out, "   (replaces the %s token %q has — revoked once the new one is verified)\n", service.Title, env.Name)
	}
	if !*noOpen {
		// Without a browser the address above is what the user goes by.
		_ = a.openURL(issue.URL)
	}
	value, err := a.readSecretValue("Paste the token (input is hidden): ")
	if err != nil {
		return err
	}
	result, err := a.setToken(ctx, env.Name, service, value, request, tokenName, refresh)
	if err != nil {
		return err
	}
	verb := "Set"
	if refresh {
		verb = "Refreshed"
	}
	fmt.Fprintf(out, "%s the %s token of %q: %s\n", verb, service.Title, env.Name, tokenSummary(result.Token, a.now()))
	if service.Name == config.ServiceGitHub {
		fmt.Fprintf(out, "  repos: %s\n", repoList(result.Token.Repos))
	}
	notes, warnings := result.report()
	for _, note := range notes {
		fmt.Fprintf(out, "  %s\n", note)
	}
	if result.SyncErr == nil {
		fmt.Fprintf(out, "  %s\n", newShells)
	}
	a.warn(warnings)
	return nil
}

func (a *app) tokenRemoveCommand(ctx context.Context, args []string) error {
	const usage = "token remove [<env>] [<service>] [--yes]"
	flags := flag.NewFlagSet("token remove", flag.ContinueOnError)
	yes := flags.Bool("yes", false, "")
	words, err := parseFlags(flags, args)
	if err != nil {
		return err
	}
	if len(words) > 2 {
		return usagef("usage: bh %s", usage)
	}
	store, env, words, err := a.tokenEnvironment(ctx, usage, words, "")
	if err != nil {
		return err
	}
	creds, err := a.credentialsOf(store, env, false)
	if err != nil {
		return err
	}
	if len(creds.Tokens) == 0 {
		return fmt.Errorf("%q has no tokens", env.Name)
	}
	if len(words) == 0 {
		picked := creds.Tokens[0].Service
		if len(creds.Tokens) > 1 {
			var choices []tui.Choice
			for _, token := range creds.Tokens {
				choices = append(choices, tui.Choice{Name: token.Service, Note: tokenSummary(token, a.now())})
			}
			if picked, err = a.choose(ctx, usage, "the service", "Which token?", choices); err != nil {
				return err
			}
		}
		words = []string{picked}
	}
	service, known := serviceNamed(words[0])
	if !known {
		return usagef("%q is not a service bh gets tokens from (%s)", words[0], serviceNames())
	}
	token := creds.token(service.Name)
	if token == nil {
		return fmt.Errorf("%q has no %s token", env.Name, service.Title)
	}
	if !*yes {
		if !a.isTerminal() {
			return errors.New("removing a token revokes it: pass --yes when not at a terminal")
		}
		question := fmt.Sprintf("Revoke the %s token of %q (%s) and take it out of the environment?", service.Title, env.Name, tokenSummary(*token, a.now()))
		if !a.confirm(question) {
			return errAborted
		}
	}
	result, err := a.removeToken(ctx, env.Name, service)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Revoked the %s token of %q and took it out\n", service.Title, env.Name)
	if result.SyncErr != nil {
		a.warn([]string{fmt.Sprintf("the revoked token is still in the environment's volume (%s) — bh up %s clears it", oneLine(result.SyncErr), env.Name)})
	}
	return nil
}
