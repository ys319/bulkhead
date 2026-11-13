package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"slices"
	"strings"

	"github.com/ys319/bulkhead/internal/config"
)

const tokenUsage = `usage: bh token <subcommand>

  token list <env>          the tokens handed to <env>, GitHub's included
                            (values are never shown)
  token set <env> hf [--no-open]
                            set HF_TOKEN: opens Hugging Face's token page, then
                            reads the token (hidden; or from stdin)
  token set <env> custom <NAME>
                            set the variable NAME (hidden; or from stdin);
                            an existing NAME is replaced
  token remove <env> <NAME> take NAME out of <env> (it is not revoked at its
                            service)

  Values take printable ASCII without spaces or '. New shells see a change;
  running shells keep the old value. The GitHub token has its own command
  (bh github). Tokens can be read by everything inside the environment —
  their scope is the only protection.`

// These are the tokens other than GitHub's: a plain second route next to
// it. bh neither issues nor checks nor revokes them; it hands them to the
// environment as environment variables and takes them out again.

// hfTokenURL is where Hugging Face tokens are made and revoked.
const hfTokenURL = "https://huggingface.co/settings/tokens"

// newShellsNote says when a change of a token is seen inside.
const newShellsNote = "new shells see it (running shells keep the old value)"

// tokenSetResult is what setting a token came to.
type tokenSetResult struct {
	Env   config.Environment
	Entry config.TokenEntry
	// Replaced: there was a token of that name before.
	Replaced bool
	// SyncErr: the token is saved but not in the environment's volume; the
	// next bh up puts it there.
	SyncErr error
}

// setToken adds a token to the environment, in place of one of the same
// name if there is one, and puts it into the environment's volume.
func (a *app) setToken(ctx context.Context, ref, kind, name, value string) (tokenSetResult, error) {
	store, err := a.store()
	if err != nil {
		return tokenSetResult{}, err
	}
	env, err := tokenTarget(store, ref)
	if err != nil {
		return tokenSetResult{}, err
	}
	result := tokenSetResult{Env: env}
	if err := config.CheckTokenName(kind, name); err != nil {
		return result, err
	}
	// A pasted value ends in a line break.
	value = strings.TrimSpace(value)
	if err := config.CheckTokenValue(value); err != nil {
		return result, err
	}
	tokens, _, err := store.LoadTokens(env.ID)
	if err != nil {
		return result, err
	}
	result.Entry = config.TokenEntry{Kind: kind, Name: name, Value: value, AddedAt: config.Timestamp(a.now())}
	// A replaced token keeps its place in the list.
	at := slices.IndexFunc(tokens, func(entry config.TokenEntry) bool { return entry.Name == name })
	if result.Replaced = at >= 0; result.Replaced {
		tokens = slices.Clone(tokens)
		tokens[at] = result.Entry
	} else {
		tokens = append(tokens, result.Entry)
	}
	if err := store.SaveTokens(env.ID, tokens); err != nil {
		return result, err
	}
	result.SyncErr = a.applyTokens(ctx, store, env.ID, volumeWriter.syncTokens)
	return result, nil
}

// report words what setting the token came to: notes on what happens now,
// and warnings about what is left to do.
func (r tokenSetResult) report() (notes, warnings []string) {
	if r.SyncErr != nil {
		return nil, []string{fmt.Sprintf("saved but not yet in the environment (%s) — bh up %s applies it", oneLine(r.SyncErr), r.Env.Name)}
	}
	return []string{newShellsNote}, nil
}

// tokenRemoveResult is what removing a token came to.
type tokenRemoveResult struct {
	Env   config.Environment
	Entry config.TokenEntry
	// SyncErr: the token is forgotten but still in the environment's
	// volume; the next bh up takes it out.
	SyncErr error
}

// removeToken takes a token out of the environment. It stays valid at its
// service: bh cannot revoke what it did not issue.
func (a *app) removeToken(ctx context.Context, ref, name string) (tokenRemoveResult, error) {
	store, err := a.store()
	if err != nil {
		return tokenRemoveResult{}, err
	}
	env, err := tokenTarget(store, ref)
	if err != nil {
		return tokenRemoveResult{}, err
	}
	result := tokenRemoveResult{Env: env}
	if name == "GH_TOKEN" {
		return result, fmt.Errorf("the GitHub token is removed by revoking it (bh github revoke %s)", env.Name)
	}
	tokens, _, err := store.LoadTokens(env.ID)
	if err != nil {
		return result, err
	}
	at := slices.IndexFunc(tokens, func(entry config.TokenEntry) bool { return entry.Name == name })
	if at < 0 {
		return result, fmt.Errorf("%q has no token named %s", env.Name, name)
	}
	result.Entry = tokens[at]
	// The file stays even when this was the last token: an empty list is
	// what makes the next write, now or at bh up, empty the volume.
	if err := store.SaveTokens(env.ID, slices.Delete(slices.Clone(tokens), at, at+1)); err != nil {
		return result, err
	}
	result.SyncErr = a.applyTokens(ctx, store, env.ID, volumeWriter.syncTokens)
	return result, nil
}

// tokenListEntry is one token of an environment, without its value.
type tokenListEntry struct {
	Kind string // "github", or one of config.TokenKinds
	Name string // the variable it is exported as
	// Summary is GitHub's token in a line; empty for the others.
	Summary string
}

// listTokens is every token handed to the environment, GitHub's first. A
// record that cannot be read does not hide the other: it is reported in
// problems.
func (a *app) listTokens(ref string) (env config.Environment, entries []tokenListEntry, problems []string, err error) {
	store, err := a.store()
	if err != nil {
		return env, nil, nil, err
	}
	if env, err = tokenHolder(store, ref); err != nil {
		return env, nil, nil, err
	}
	if state, err := store.LoadGitHub(env.ID); err != nil {
		problems = append(problems, err.Error())
	} else if state != nil {
		entries = append(entries, tokenListEntry{Kind: "github", Name: "GH_TOKEN", Summary: githubSummary(*state, a.now())})
	}
	if tokens, _, err := store.LoadTokens(env.ID); err != nil {
		problems = append(problems, err.Error())
	} else {
		for _, token := range tokens {
			entries = append(entries, tokenListEntry{Kind: token.Kind, Name: token.Name})
		}
	}
	return env, entries, problems, nil
}

// tokensDestroyNote names the tokens that outlive the environment being
// destroyed: they are still valid at their services, where only their owner
// can revoke them. Empty when there are none.
func tokensDestroyNote(store config.Store, id string) string {
	tokens, _, err := store.LoadTokens(id)
	if err != nil {
		return "the token record could not be read — revoke its tokens at their services by hand if needed"
	}
	if len(tokens) == 0 {
		return ""
	}
	names := make([]string, len(tokens))
	for i, token := range tokens {
		names[i] = token.Name
		if token.Kind == config.TokenKindHF {
			names[i] += " (" + hfTokenURL + ")"
		}
	}
	return "tokens are still valid at their services — revoke by hand if needed: " + strings.Join(names, ", ")
}

// tokenLeftovers deals with the tokens of an environment that is being
// destroyed, before its records go: the GitHub token is revoked, the others
// are named. What comes back are warnings; none of it stops the destruction.
func (a *app) tokenLeftovers(ctx context.Context, store config.Store, id string) []string {
	var warnings []string
	for _, warning := range []string{a.revokeBeforeDestroy(ctx, store, id), tokensDestroyNote(store, id)} {
		if warning != "" {
			warnings = append(warnings, warning)
		}
	}
	return warnings
}

func (a *app) token(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("%s", tokenUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		if len(rest) != 1 {
			return usagef("usage: bh token list <env>")
		}
		env, entries, problems, err := a.listTokens(rest[0])
		if err != nil {
			return err
		}
		if len(entries) == 0 && len(problems) == 0 {
			fmt.Fprintf(a.stdout, "%q has no tokens (bh token set %s hf | custom <NAME>; GitHub: bh github set)\n", env.Name, env.Name)
			return nil
		}
		width := 16
		for _, entry := range entries {
			width = max(width, len(entry.Name)+2)
		}
		for _, entry := range entries {
			what := entry.Kind
			if entry.Summary != "" {
				what += " · " + entry.Summary
			}
			fmt.Fprintf(a.stdout, "%-*s%s\n", width, entry.Name, what)
		}
		if len(problems) > 0 {
			return errors.New(strings.Join(problems, "\n"))
		}
		return nil

	case "set":
		return a.tokenSet(ctx, rest)

	case "remove":
		if len(rest) != 2 {
			return usagef("usage: bh token remove <env> <NAME>")
		}
		result, err := a.removeToken(ctx, rest[0], rest[1])
		if err != nil {
			return err
		}
		where := ""
		if result.Entry.Kind == config.TokenKindHF {
			where = " — " + hfTokenURL
		}
		fmt.Fprintf(a.stdout, "Removed %s from %q — new shells no longer see it (running shells keep it; it is not revoked at its service%s)\n",
			result.Entry.Name, result.Env.Name, where)
		if result.SyncErr != nil {
			a.warn([]string{fmt.Sprintf("the environment's volume still has it (%s) — bh up %s clears it", oneLine(result.SyncErr), result.Env.Name)})
		}
		return nil
	}
	return usagef("%s", tokenUsage)
}

func (a *app) tokenSet(ctx context.Context, args []string) error {
	const usage = "usage: bh token set <env> hf [--no-open] | bh token set <env> custom <NAME>"
	flags := flag.NewFlagSet("token set", flag.ContinueOnError)
	noOpen := flags.Bool("no-open", false, "")
	words, err := parseFlags(flags, args)
	if err != nil {
		return err
	}
	var kind, name string
	switch {
	case len(words) == 2 && words[1] == config.TokenKindHF:
		kind, name = config.TokenKindHF, config.HFTokenName
	case len(words) == 3 && words[1] == config.TokenKindCustom:
		kind, name = config.TokenKindCustom, words[2]
	default:
		return usagef("%s", usage)
	}
	store, err := a.store()
	if err != nil {
		return err
	}
	env, err := tokenTarget(store, words[0])
	if err != nil {
		return err
	}
	// The name is checked before the token is asked for: being turned down
	// for the name after pasting means pasting twice.
	if err := config.CheckTokenName(kind, name); err != nil {
		return err
	}
	if kind == config.TokenKindHF {
		fmt.Fprintf(a.stdout, "1. On Hugging Face, create a token for %q:\n", env.Name)
		fmt.Fprintf(a.stdout, "   %s\n", hfTokenURL)
		fmt.Fprintln(a.stdout, "2. Paste the token below.")
		if !*noOpen {
			// Without a browser the address above is what the user goes by.
			_ = a.openURL(hfTokenURL)
		}
	}
	value, err := a.readToken()
	if err != nil {
		return err
	}
	result, err := a.setToken(ctx, env.Name, kind, name, value)
	if err != nil {
		return err
	}
	verb := "Set"
	if result.Replaced {
		verb = "Replaced"
	}
	notes, warnings := result.report()
	line := fmt.Sprintf("%s %s for %q", verb, name, env.Name)
	if len(notes) > 0 {
		line += " — " + strings.Join(notes, "; ")
	}
	fmt.Fprintln(a.stdout, line)
	a.warn(warnings)
	return nil
}
