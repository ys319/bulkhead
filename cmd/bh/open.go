package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/plan"
	"github.com/ys319/bulkhead/internal/sshconfig"
)

// The programs an environment can be opened in.
const (
	inVSCode = "vscode"
	inClaude = "claude"
)

const openUsage = "usage: bh open <env> [--in vscode|claude]"

// hasVSCode: the `code` command is in PATH.
func (a *app) hasVSCode() bool {
	_, err := a.lookPath("code")
	return err == nil
}

// hasClaude: the Claude desktop app is on this machine, as far as that can
// be told without asking the desktop. It decides only whether the
// full-screen interface offers it: `bh open --in claude` tries regardless.
//
// On macOS the application is looked for where applications are put. On
// Windows nothing short of the registry says who takes claude:// links, so
// it is offered; elsewhere it is not.
func (a *app) hasClaude() bool {
	switch a.goos {
	case "darwin":
		for _, dir := range []string{"/Applications", filepath.Join(a.getenv("HOME"), "Applications")} {
			if _, err := os.Stat(filepath.Join(dir, "Claude.app")); err == nil {
				return true
			}
		}
		return false
	case "windows":
		return true
	}
	return false
}

// claudeLink is the link that opens Claude Desktop's new-session page with
// an SSH connection filled in (Claude Desktop 2.110.0 or later; earlier
// versions open the page and ignore the connection).
//
// The host is the alias of the Host entry bh keeps in the ssh client's
// configuration, without user or port: key, port, user and the name the
// host key is filed under all come from that entry, as they do for VS Code.
// The link itself can carry no key. The desktop asks before it uses or
// saves any of this.
func claudeLink(alias, folder string) string {
	// As in the form the documentation shows: a / stays as it is (it needs
	// no escaping in a query) and a space is %20, which every reader takes;
	// a + is a space only to some.
	escape := func(value string) string {
		return strings.NewReplacer("%2F", "/", "+", "%20").Replace(url.QueryEscape(value))
	}
	return "claude://code/new?ssh_host=" + escape(alias) + "&ssh_folder=" + escape(folder)
}

// openLink hands a link to the program this desktop has for its scheme.
func (a *app) openLink(link string) error {
	if a.goos == "darwin" {
		// open answers, and says so when no application takes the scheme.
		_, err := a.tool("open", []string{link})
		return err
	}
	return a.openURL(link)
}

// openIn opens the environment in VS Code or in Claude Desktop. Both reach
// it through the ssh client's configuration: key, port and user come from
// the Host entry bh keeps there.
func (a *app) openIn(ctx context.Context, store config.Store, env config.Environment, where string) error {
	if where == inVSCode && !a.hasVSCode() {
		return errors.New(`VS Code's code command was not found in PATH (in VS Code, run "Shell Command: Install 'code' command in PATH")`)
	}
	if err := a.requireRunning(ctx, env); err != nil {
		return err
	}
	if _, err := ensureMachineKey(store); err != nil {
		return err
	}
	// The entry is what the program connects through, so here a failure counts.
	if err := a.writeSSH(store); err != nil {
		return fmt.Errorf("the ssh configuration could not be updated: %w", err)
	}
	alias := sshconfig.Alias(env.Name)
	if where == inClaude {
		if err := a.openLink(claudeLink(alias, plan.Workspace)); err != nil {
			return fmt.Errorf("the link for Claude Desktop was not taken (is it installed?): %w", err)
		}
		return nil
	}
	// The folder is what makes this a window of its own: with the remote
	// alone, a VS Code that is already open is brought to the front and
	// nothing else happens.
	_, err := a.tool("code", []string{"--remote", "ssh-remote+" + alias, plan.Workspace})
	return err
}

func (a *app) open(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("open", flag.ContinueOnError)
	where := flags.String("in", inVSCode, "")
	rest, err := parseFlags(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef(openUsage)
	}
	if *where != inVSCode && *where != inClaude {
		return usagef("--in: %q is neither vscode nor claude", *where)
	}
	store, cfg, err := a.load()
	if err != nil {
		return err
	}
	env, err := cfg.Find(rest[0])
	if err != nil {
		return err
	}
	if err := a.openIn(ctx, store, env, *where); err != nil {
		return err
	}
	if *where == inClaude {
		// The desktop asks before it connects: nothing is open yet.
		fmt.Fprintf(a.stdout, "Handed %q to Claude Desktop as %s — confirm the connection there\n", env.Name, sshconfig.Alias(env.Name))
		return nil
	}
	fmt.Fprintf(a.stdout, "Opened %q in VS Code\n", env.Name)
	return nil
}
