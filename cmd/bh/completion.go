package main

import (
	"context"
	"fmt"
	"strings"
)

// `bh completion zsh|bash` prints a completion script for the shell's rc
// file to eval. Nothing is written anywhere: an installed file would go
// stale with the next bh, a script made by the binary that is in PATH
// cannot.
//
// The words come from the tables below. The commands themselves are the
// map in main.go; a command added there is added here too (the tests hold
// the two against each other).

// Which environments a word on the command line names; "" is none.
const (
	envsAll    = "all"
	envsLocal  = "local"
	envsRemote = "remote"
)

// completedWord is a subcommand (or a fixed argument) with what the shell
// shows next to it. envs is what the argument after it names.
//
// The descriptions go into zsh's 'name:description' and 'name[description]'
// forms unquoted: no colon, apostrophe or square bracket in them.
type completedWord struct {
	name, about string
	envs        string
}

// completedCommand is a command of bh as the completion knows it.
type completedCommand struct {
	name, about string
	// envs is what the first argument names.
	envs string
	// flags are offered when the word under the cursor starts with a dash.
	flags []string
	// subs are the words the first argument is chosen from.
	subs []completedWord
}

var completedCommands = []completedCommand{
	{name: "create", about: "define a new environment", flags: []string{"--from", "--port", "--publish", "--restart"}},
	{name: "configure", about: "change settings", envs: envsLocal,
		flags: []string{"--publish", "--port", "--restart", "--gpu", "--cdi-devices", "--docker-socket"}},
	{name: "rename", about: "give an environment another name", envs: envsAll},
	{name: "up", about: "start, creating what is missing", envs: envsLocal},
	{name: "down", about: "stop (--all for every local one)", envs: envsLocal, flags: []string{"--all"}},
	{name: "restart", about: "stop, recreate both containers, start", envs: envsLocal,
		flags: []string{"--rebuild", "--pull", "--no-cache"}},
	{name: "list", about: "all environments and their state"},
	{name: "status", about: "one environment in detail", envs: envsAll},
	{name: "ssh", about: "open a shell, or run a command", envs: envsAll},
	{name: "open", about: "open in VS Code", envs: envsAll},
	{name: "key", about: "the machine key and other keys that may log in", flags: []string{"--label", "--yes"},
		subs: []completedWord{
			{name: "show", about: "print the public key of this machine"},
			{name: "list", about: "the registered keys or those of an environment", envs: envsLocal},
			{name: "add", about: "register a public key"},
			{name: "rename", about: "change the label of a registered key"},
			{name: "remove", about: "remove from the registry"},
			{name: "attach", about: "let a key log in to an environment", envs: envsLocal},
			{name: "detach", about: "take that back", envs: envsLocal},
		}},
	{name: "remote", about: "environments that run on another machine", flags: []string{"--name", "--host", "--port"},
		subs: []completedWord{
			{name: "add", about: "register a remote environment"},
			{name: "configure", about: "change host or port", envs: envsRemote},
			{name: "remove", about: "forget it (the other machine is not touched)", envs: envsRemote},
		}},
	{name: "logs", about: "follow the output of both containers", envs: envsLocal,
		flags: []string{"--tail", "--no-follow", "--timestamps"}},
	{name: "init", about: "run init.sh in the running environment", envs: envsLocal},
	{name: "root-shell", about: "root shell in the running dev container", envs: envsLocal},
	{name: "gpu", about: "the GPU devices of the engine",
		subs: []completedWord{{name: "list", about: "list the CDI devices"}}},
	{name: "github", about: "GitHub token of an environment",
		flags: []string{"--preset", "--owner", "--days", "--no-open", "--yes"},
		subs: []completedWord{
			{name: "set", about: "issue a fine-grained token", envs: envsLocal},
			{name: "status", about: "preset, repositories and expiry", envs: envsLocal},
			{name: "revoke", about: "revoke the token and remove it", envs: envsLocal},
		}},
	{name: "token", about: "other tokens (Hugging Face, custom)", flags: []string{"--no-open"},
		subs: []completedWord{
			{name: "list", about: "the tokens of an environment", envs: envsLocal},
			{name: "set", about: "set a token (hf or custom NAME)", envs: envsLocal},
			{name: "remove", about: "remove a token (it is not revoked)", envs: envsLocal},
		}},
	{name: "doctor", about: "check this machine and the boundary of the containers", envs: envsLocal},
	{name: "path", about: "print the configuration directory", envs: envsAll},
	{name: "snapshot", about: "the data of an environment in one file", flags: []string{"--note"},
		subs: []completedWord{
			{name: "create", about: "take a snapshot of a stopped environment", envs: envsLocal},
			{name: "list", about: "the snapshots of one environment or of all", envs: envsLocal},
			{name: "remove", about: "delete a snapshot", envs: envsLocal},
		}},
	{name: "rollback", about: "put the data of a snapshot back", envs: envsLocal, flags: []string{"--only", "--no-snapshot", "--yes"}},
	{name: "export", about: "write a snapshot to a file of your choosing", envs: envsLocal},
	{name: "import", about: "make a new environment from a snapshot file"},
	{name: "archive", about: "take a snapshot, then remove containers and volumes", envs: envsLocal, flags: []string{"--yes"}},
	{name: "unarchive", about: "bring an archived environment back", envs: envsLocal},
	{name: "reset", about: "take the tools back to the image, or the home directory too", envs: envsLocal, flags: []string{"--home", "--no-snapshot", "--yes"}},
	{name: "destroy", about: "remove the environment and its data", envs: envsAll, flags: []string{"--yes"}},
	{name: "migrate", about: "take over an environment of the TypeScript version", flags: []string{"--yes", "--undo"}},
	{name: "version", about: "the build, and the Docker Engine bh talks to"},
	{name: "completion", about: "print a shell completion script",
		subs: []completedWord{{name: "zsh"}, {name: "bash"}}},
	{name: "help", about: "show usage"},
}

// What stands after a flag that takes one of a few values. --publish and
// --docker-socket take on|off for configure only (create --publish takes
// nothing); the scripts handle those two apart.
var completedValues = []struct {
	flag   string
	values []string
}{
	{"--restart", []string{"unless-stopped", "no"}},
	{"--gpu", []string{"off", "legacy", "cdi"}},
	{"--preset", []string{"read", "push", "push+pr"}},
}

// tokenKinds stand after `bh token set <env>`.
var tokenKinds = []completedWord{
	{name: "hf", about: "Hugging Face (HF_TOKEN)"},
	{name: "custom", about: "a variable you name"},
}

// topLevelFlags are offered for a dash where the command goes. The short
// forms -V and -h work and are not offered: nobody completes two letters.
var topLevelFlags = []string{"--version", "--help"}

func (a *app) completion(_ context.Context, args []string) error {
	if len(args) != 1 {
		return usagef("usage: bh completion zsh|bash")
	}
	script, err := completionScript(args[0])
	if err != nil {
		return err
	}
	fmt.Fprint(a.stdout, script)
	return nil
}

func completionScript(shell string) (string, error) {
	switch shell {
	case "zsh":
		return zshCompletion(), nil
	case "bash":
		return bashCompletion(), nil
	}
	return "", usagef("completion: %q is not a supported shell (zsh, bash)", shell)
}

// completeNames is `bh __complete envs [local|remote]`: what the scripts
// call while the user is typing, to get the names of the environments. It
// answers from the configuration directory alone — no Docker, so it is
// immediate — and on any problem it prints nothing: an error message in
// the middle of a command line being typed helps nobody. It is not in the
// usage text; it is the scripts' business.
func (a *app) completeNames(_ context.Context, args []string) error {
	if len(args) == 0 || args[0] != "envs" {
		return nil
	}
	kind := envsAll
	if len(args) > 1 {
		kind = args[1]
	}
	_, cfg, err := a.load()
	if err != nil {
		return nil
	}
	for _, env := range cfg.Envs {
		if (kind == envsLocal && !env.Local()) || (kind == envsRemote && env.Local()) {
			continue
		}
		fmt.Fprintln(a.stdout, env.Name)
	}
	return nil
}

// commandsNaming are the commands whose first argument names an
// environment of the kind, as a shell pattern: "up|down|restart".
func commandsNaming(kind string) string {
	var names []string
	for _, command := range completedCommands {
		if command.envs == kind {
			names = append(names, command.name)
		}
	}
	return strings.Join(names, "|")
}

// subsNaming are the subcommands of a command whose argument names an
// environment, as shell patterns by kind of environment.
func subsNaming(command completedCommand) (cases []struct{ pattern, kind string }) {
	for _, kind := range envKinds {
		var names []string
		for _, sub := range command.subs {
			if sub.envs == kind {
				names = append(names, sub.name)
			}
		}
		if len(names) > 0 {
			cases = append(cases, struct{ pattern, kind string }{strings.Join(names, "|"), kind})
		}
	}
	return cases
}

func wordNames(words []completedWord) string {
	list := make([]string, len(words))
	for i, word := range words {
		list[i] = word.name
	}
	return strings.Join(list, " ")
}

// zshValues is the argument list of _values: 'name[description]'.
func zshValues(words []completedWord) string {
	list := make([]string, len(words))
	for i, word := range words {
		if word.about == "" {
			list[i] = word.name
		} else {
			list[i] = "'" + word.name + "[" + word.about + "]'"
		}
	}
	return strings.Join(list, " ")
}

var envKinds = []string{envsAll, envsLocal, envsRemote}

func zshCompletion() string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	line(`# bh completion (zsh) — add to your ~/.zshrc:`)
	line(`#   eval "$(bh completion zsh)"`)
	line(``)
	line(`_bh_envs() {`)
	line(`  local -a names`)
	line(`  names=(${(f)"$(command bh __complete envs $1 2>/dev/null)"})`)
	line(`  (( ${#names} )) && _describe -t environments 'environment' names`)
	line(`}`)
	line(``)
	line(`_bh() {`)
	line(`  local -a commands=(`)
	for _, command := range completedCommands {
		line(`    '%s:%s'`, command.name, command.about)
	}
	line(`  )`)
	line(`  if (( CURRENT == 2 )); then`)
	line(`    if [[ ${words[CURRENT]} == -* ]]; then`)
	line(`      compadd -- %s`, strings.Join(topLevelFlags, " "))
	line(`    else`)
	line(`      _describe -t commands 'bh command' commands`)
	line(`    fi`)
	line(`    return`)
	line(`  fi`)
	line(`  local cmd=${words[2]}`)
	line(`  case ${words[CURRENT-1]} in`)
	line(`    --from) _bh_envs local; return ;;`)
	for _, entry := range completedValues {
		line(`    %s) compadd -- %s; return ;;`, entry.flag, strings.Join(entry.values, " "))
	}
	line(`    --publish|--docker-socket)`)
	line(`      if [[ $cmd == configure ]]; then`)
	line(`        compadd -- on off`)
	line(`        return`)
	line(`      fi`)
	line(`      ;;`)
	line(`  esac`)
	line(`  if [[ ${words[CURRENT]} == -* ]]; then`)
	line(`    case $cmd in`)
	for _, command := range completedCommands {
		if len(command.flags) > 0 {
			line(`      %s) compadd -- %s ;;`, command.name, strings.Join(command.flags, " "))
		}
	}
	line(`    esac`)
	line(`    return`)
	line(`  fi`)
	line(`  case $cmd in`)
	for _, kind := range envKinds {
		if pattern := commandsNaming(kind); pattern != "" {
			line(`    %s)`, pattern)
			line(`      (( CURRENT == 3 )) && _bh_envs %s`, kind)
			line(`      ;;`)
		}
	}
	for _, command := range completedCommands {
		if len(command.subs) == 0 {
			continue
		}
		line(`    %s)`, command.name)
		line(`      if (( CURRENT == 3 )); then`)
		line(`        _values '%s command' %s`, command.name, zshValues(command.subs))
		if cases := subsNaming(command); len(cases) > 0 {
			line(`      elif (( CURRENT == 4 )); then`)
			line(`        case ${words[3]} in`)
			for _, entry := range cases {
				line(`          %s) _bh_envs %s ;;`, entry.pattern, entry.kind)
			}
			line(`        esac`)
		}
		if command.name == "token" {
			line(`      elif (( CURRENT == 5 )) && [[ ${words[3]} == set ]]; then`)
			line(`        _values 'token kind' %s`, zshValues(tokenKinds))
		}
		line(`      fi`)
		line(`      ;;`)
	}
	line(`  esac`)
	line(`}`)
	line(``)
	line(`compdef _bh bh`)
	return b.String()
}

func bashCompletion() string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	commandNames := make([]string, len(completedCommands))
	for i, command := range completedCommands {
		commandNames[i] = command.name
	}

	line(`# bh completion (bash) — add to your ~/.bashrc:`)
	line(`#   eval "$(bh completion bash)"`)
	line(``)
	line(`_bh_envs() {`)
	line(`  command bh __complete envs "$1" 2>/dev/null`)
	line(`}`)
	line(``)
	line(`_bh() {`)
	line(`  local cur prev cmd`)
	line(`  cur=${COMP_WORDS[COMP_CWORD]}`)
	line(`  prev=${COMP_WORDS[COMP_CWORD-1]}`)
	line(`  cmd=${COMP_WORDS[1]}`)
	line(`  COMPREPLY=()`)
	line(`  if (( COMP_CWORD == 1 )); then`)
	line(`    if [[ $cur == -* ]]; then`)
	line(`      COMPREPLY=($(compgen -W "%s" -- "$cur"))`, strings.Join(topLevelFlags, " "))
	line(`    else`)
	line(`      COMPREPLY=($(compgen -W "%s" -- "$cur"))`, strings.Join(commandNames, " "))
	line(`    fi`)
	line(`    return`)
	line(`  fi`)
	line(`  case $prev in`)
	line(`    --from) COMPREPLY=($(compgen -W "$(_bh_envs local)" -- "$cur")); return ;;`)
	for _, entry := range completedValues {
		line(`    %s) COMPREPLY=($(compgen -W "%s" -- "$cur")); return ;;`, entry.flag, strings.Join(entry.values, " "))
	}
	line(`    --publish|--docker-socket)`)
	line(`      if [[ $cmd == configure ]]; then`)
	line(`        COMPREPLY=($(compgen -W "on off" -- "$cur"))`)
	line(`        return`)
	line(`      fi`)
	line(`      ;;`)
	line(`  esac`)
	line(`  if [[ $cur == -* ]]; then`)
	line(`    local flags=""`)
	line(`    case $cmd in`)
	for _, command := range completedCommands {
		if len(command.flags) > 0 {
			line(`      %s) flags="%s" ;;`, command.name, strings.Join(command.flags, " "))
		}
	}
	line(`    esac`)
	line(`    COMPREPLY=($(compgen -W "$flags" -- "$cur"))`)
	line(`    return`)
	line(`  fi`)
	line(`  case $cmd in`)
	for _, kind := range envKinds {
		if pattern := commandsNaming(kind); pattern != "" {
			line(`    %s)`, pattern)
			line(`      if (( COMP_CWORD == 2 )); then`)
			line(`        COMPREPLY=($(compgen -W "$(_bh_envs %s)" -- "$cur"))`, kind)
			line(`      fi`)
			line(`      ;;`)
		}
	}
	for _, command := range completedCommands {
		if len(command.subs) == 0 {
			continue
		}
		line(`    %s)`, command.name)
		line(`      if (( COMP_CWORD == 2 )); then`)
		line(`        COMPREPLY=($(compgen -W "%s" -- "$cur"))`, wordNames(command.subs))
		if cases := subsNaming(command); len(cases) > 0 {
			line(`      elif (( COMP_CWORD == 3 )); then`)
			line(`        case ${COMP_WORDS[2]} in`)
			for _, entry := range cases {
				line(`          %s) COMPREPLY=($(compgen -W "$(_bh_envs %s)" -- "$cur")) ;;`, entry.pattern, entry.kind)
			}
			line(`        esac`)
		}
		if command.name == "token" {
			line(`      elif (( COMP_CWORD == 4 )) && [[ ${COMP_WORDS[2]} == set ]]; then`)
			line(`        COMPREPLY=($(compgen -W "%s" -- "$cur"))`, wordNames(tokenKinds))
		}
		line(`      fi`)
		line(`      ;;`)
	}
	line(`  esac`)
	line(`}`)
	line(``)
	line(`complete -F _bh bh`)
	return b.String()
}
