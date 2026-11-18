package main

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/snapshot"
)

// `bh completion zsh|bash|powershell` prints a completion script for the shell's rc
// file to eval. Nothing is written anywhere: an installed file would go
// stale with the next bh, a script made by the binary that is in PATH
// cannot.
//
// The script is only a way in. What to offer is decided here, by the
// program itself: the shell hands the command line as typed so far to
// `bh __complete`, and bh answers with the candidates for the word under
// the cursor — from the table below and from the configuration directory
// (the environments, their variables, snapshots, tokens). Completion is
// how arguments are chosen: bh does not ask for what was left out
// (decided 2026-11-13).
//
// The commands themselves are the map in main.go; a command added there
// is added here too (the tests hold the two against each other).

// candidate is a word to offer, with what the shell shows next to it.
type candidate struct{ name, about string }

// completing is what a position's candidates can depend on.
type completing struct {
	a *app
	// given are the positional words before the cursor, after the command
	// and its subcommand: given[0] is usually the environment.
	given []string
}

// argument makes the candidates for one position. nil stands for a
// position where anything goes (a new name, a value).
type argument func(c *completing) []candidate

// fileNames stands for "a file's name": the shell completes those itself.
var fileNamesMark = []candidate{{name: "\x00files"}}

func fileNames(*completing) []candidate { return fileNamesMark }

func oneOf(list ...string) argument {
	return func(*completing) []candidate {
		out := make([]candidate, len(list))
		for i, word := range list {
			out[i] = candidate{name: word}
		}
		return out
	}
}

// environments are the names of the environments that pass.
func environments(pass func(config.Environment) bool) argument {
	return func(c *completing) []candidate {
		_, cfg, err := c.a.load()
		if err != nil {
			return nil
		}
		var out []candidate
		for _, env := range cfg.Envs {
			if pass(env) {
				about := ""
				switch {
				case !env.Local():
					about = "remote"
				case env.Archived != "":
					about = "archived"
				}
				out = append(out, candidate{env.Name, about})
			}
		}
		return out
	}
}

var (
	anyEnvs      = environments(func(config.Environment) bool { return true })
	localEnvs    = environments(config.Environment.Local)
	remoteEnvs   = environments(func(env config.Environment) bool { return !env.Local() })
	awakeEnvs    = environments(func(env config.Environment) bool { return env.Local() && env.Archived == "" })
	archivedEnvs = environments(func(env config.Environment) bool { return env.Archived != "" })
)

// of looks the environment named first up; false when it is not there.
func (c *completing) of() (config.Store, config.Environment, bool) {
	if len(c.given) == 0 {
		return config.Store{}, config.Environment{}, false
	}
	store, cfg, err := c.a.load()
	if err != nil {
		return store, config.Environment{}, false
	}
	env, err := cfg.Find(c.given[0])
	return store, env, err == nil
}

func variableNames(c *completing) []candidate {
	_, env, found := c.of()
	if !found {
		return nil
	}
	names := make([]string, 0, len(env.Vars))
	for name := range env.Vars {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]candidate, len(names))
	for i, name := range names {
		out[i] = candidate{name: name}
	}
	return out
}

// Records only, and only today's: completing reads no value and takes
// nothing over.
func secretNames(c *completing) []candidate {
	store, env, found := c.of()
	if !found {
		return nil
	}
	secrets, err := store.LoadSecrets(env.ID)
	if err != nil {
		return nil
	}
	out := make([]candidate, len(secrets))
	for i, secret := range secrets {
		out[i] = candidate{name: secret.Name}
	}
	return out
}

// serviceWords are the services a token can be had from.
func serviceWords(*completing) []candidate {
	out := make([]candidate, len(tokenServices))
	for i, service := range tokenServices {
		out[i] = candidate{service.Name, service.Title}
	}
	return out
}

// presetWords are the presets of the service named second; none for a
// service without them.
func presetWords(c *completing) []candidate {
	if len(c.given) < 2 {
		return nil
	}
	service, known := serviceNamed(c.given[1])
	if !known {
		return nil
	}
	out := make([]candidate, len(service.Presets))
	for i, preset := range service.Presets {
		out[i] = candidate{preset.Name, preset.About}
	}
	return out
}

// heldTokens are the services the environment has a token of.
func heldTokens(c *completing) []candidate {
	store, env, found := c.of()
	if !found {
		return nil
	}
	tokens, err := store.LoadTokens(env.ID)
	if err != nil {
		return nil
	}
	out := make([]candidate, len(tokens))
	for i, token := range tokens {
		out[i] = candidate{token.Service, tokenSummary(token, c.a.now())}
	}
	return out
}

// snapshotNames are the environment's snapshots, newest first; with
// latest, that word leads.
func snapshotNames(latest bool) argument {
	return func(c *completing) []candidate {
		store, env, found := c.of()
		if !found {
			return nil
		}
		list, err := listSnapshots(store, env.ID)
		if err != nil {
			return nil
		}
		var out []candidate
		if latest && len(list) > 0 {
			out = append(out, candidate{"latest", "the newest snapshot"})
		}
		for _, s := range slices.Backward(list) {
			about := ""
			if s.Err == nil {
				about = s.Manifest.Kind
				if s.Manifest.Note != "" {
					about += " — " + s.Manifest.Note
				}
			}
			out = append(out, candidate{strings.TrimSuffix(s.Name, snapshot.Extension), about})
		}
		return out
	}
}

// keyNames are the registered keys by label (a key without one, or with a
// label a shell would split, by its fingerprint); with attached, only
// those the environment lets in.
func keyNames(attached bool) argument {
	return func(c *completing) []candidate {
		store, err := c.a.store()
		if err != nil {
			return nil
		}
		registry, err := store.LoadKeys()
		if err != nil {
			return nil
		}
		var allowed []string
		if attached {
			_, env, found := c.of()
			if !found {
				return nil
			}
			allowed = env.AuthorizedKeys
		}
		var out []candidate
		for _, key := range registry {
			if attached && !slices.Contains(allowed, key.Fingerprint) {
				continue
			}
			if key.Label != "" && !strings.ContainsAny(key.Label, " \t") {
				out = append(out, candidate{key.Label, key.Fingerprint})
			} else {
				out = append(out, candidate{key.Fingerprint, key.Label})
			}
		}
		return out
	}
}

// flagSpec is a flag. takes is true for one that is followed by a value;
// values makes the candidates for that value (nil: anything goes).
type flagSpec struct {
	name   string
	takes  bool
	values argument
}

func plain(name string) flagSpec { return flagSpec{name: name} }
func valued(name string, values argument) flagSpec {
	return flagSpec{name: name, takes: true, values: values}
}

var (
	onOrOff    = oneOf("on", "off")
	restarts   = oneOf("unless-stopped", "no")
	yesFlag    = plain("--yes")
	remoteFlag = []flagSpec{valued("--name", nil), valued("--host", nil), valued("--port", nil)}
)

// commandSpec is a command (or a subcommand) of bh as the completion
// knows it.
//
// The descriptions go into zsh's 'name:description' form: no colon in them.
type commandSpec struct {
	name, about string
	// args are the positions after the command, in order.
	args  []argument
	flags []flagSpec
	// subs are the subcommands the first word is chosen from.
	subs []commandSpec
}

// helpTopics are the commands bh help can be asked about. The table cannot
// name itself while it is being built, so the list is handed over at start.
func helpTopics(*completing) []candidate { return topics() }

var topics func() []candidate

func init() {
	topics = func() []candidate {
		var out []candidate
		for _, entry := range commandCandidates(completedCommands) {
			if entry.name != "help" {
				out = append(out, entry)
			}
		}
		return out
	}
}

var completedCommands = []commandSpec{
	{name: "create", about: "define a new environment", args: []argument{nil},
		flags: []flagSpec{valued("--from", localEnvs), valued("--port", nil), plain("--publish"), valued("--restart", restarts)}},
	{name: "configure", about: "change settings", args: []argument{localEnvs},
		flags: []flagSpec{valued("--publish", onOrOff), valued("--port", nil), valued("--restart", restarts),
			valued("--gpu", oneOf("off", "legacy", "cdi")), valued("--cdi-devices", nil), valued("--docker-socket", onOrOff)}},
	{name: "rename", about: "give an environment another name", args: []argument{anyEnvs, nil}},
	{name: "up", about: "start, creating what is missing", args: []argument{awakeEnvs}},
	{name: "down", about: "stop (--all for every local one)", args: []argument{awakeEnvs}, flags: []flagSpec{plain("--all")}},
	{name: "restart", about: "stop, recreate both containers, start", args: []argument{awakeEnvs},
		flags: []flagSpec{plain("--rebuild"), plain("--pull"), plain("--no-cache")}},
	{name: "list", about: "all environments and their state"},
	{name: "status", about: "one environment in detail", args: []argument{anyEnvs}},
	{name: "ssh", about: "open a shell, or run a command", args: []argument{anyEnvs}},
	{name: "open", about: "open in VS Code or the Claude desktop app", args: []argument{anyEnvs},
		flags: []flagSpec{valued("--in", oneOf(inVSCode, inClaude))}},
	{name: "key", about: "the machine key and other keys that may log in", subs: []commandSpec{
		{name: "show", about: "print the public key of this machine"},
		{name: "list", about: "the registered keys or those of an environment", args: []argument{localEnvs}},
		{name: "add", about: "register a public key", flags: []flagSpec{valued("--label", nil)}},
		{name: "rename", about: "change the label of a registered key", args: []argument{keyNames(false), nil}},
		{name: "remove", about: "remove from the registry", args: []argument{keyNames(false)}, flags: []flagSpec{yesFlag}},
		{name: "attach", about: "let a key log in to an environment", args: []argument{localEnvs, keyNames(false)}},
		{name: "detach", about: "take that back", args: []argument{localEnvs, keyNames(true)}},
	}},
	{name: "remote", about: "environments that run on another machine", subs: []commandSpec{
		{name: "add", about: "register a remote environment", args: []argument{nil}, flags: remoteFlag},
		{name: "configure", about: "change host or port", args: []argument{remoteEnvs}, flags: remoteFlag},
		{name: "remove", about: "forget it (the other machine is not touched)", args: []argument{remoteEnvs}},
	}},
	{name: "logs", about: "follow the output of both containers", args: []argument{awakeEnvs},
		flags: []flagSpec{valued("--tail", nil), plain("--no-follow"), plain("--timestamps")}},
	{name: "init", about: "run init.sh in the running environment", args: []argument{awakeEnvs}},
	{name: "root-shell", about: "root shell in the running dev container", args: []argument{awakeEnvs}},
	{name: "gpu", about: "the GPU devices of the engine", subs: []commandSpec{{name: "list", about: "list the CDI devices"}}},
	{name: "var", about: "environment variables that are no secret", subs: []commandSpec{
		{name: "list", about: "the variables of an environment", args: []argument{localEnvs}},
		{name: "set", about: "set a variable", args: []argument{localEnvs, variableNames, nil}},
		{name: "unset", about: "take a variable out", args: []argument{localEnvs, variableNames}},
	}},
	{name: "secret", about: "secret values of yours, handed in as variables", subs: []commandSpec{
		{name: "list", about: "the names of the secrets of an environment", args: []argument{localEnvs}},
		{name: "set", about: "set a secret (the value is read hidden)", args: []argument{awakeEnvs, secretNames}},
		{name: "remove", about: "take a secret out", args: []argument{awakeEnvs, secretNames}},
		{name: "store", about: "where this machine keeps the values", args: []argument{oneOf("keychain", "file")}},
	}},
	{name: "token", about: "credentials a service issues (GitHub, Hugging Face)", subs: []commandSpec{
		{name: "list", about: "the tokens of an environment or of all", args: []argument{localEnvs}},
		{name: "set", about: "get a token from a service", args: []argument{awakeEnvs, serviceWords, presetWords},
			flags: []flagSpec{valued("--owner", nil), valued("--days", nil), plain("--no-open")}},
		{name: "refresh", about: "the same token again with a new value", args: []argument{awakeEnvs, heldTokens},
			flags: []flagSpec{plain("--no-open")}},
		{name: "remove", about: "revoke a token and take it out", args: []argument{localEnvs, heldTokens}, flags: []flagSpec{yesFlag}},
	}},
	{name: "doctor", about: "check this machine and the boundary of the containers", args: []argument{localEnvs}},
	{name: "path", about: "print the configuration directory", args: []argument{anyEnvs}},
	{name: "snapshot", about: "the data of an environment in one file", subs: []commandSpec{
		{name: "create", about: "take a snapshot of a stopped environment", args: []argument{awakeEnvs}, flags: []flagSpec{valued("--note", nil)}},
		{name: "list", about: "the snapshots of one environment or of all", args: []argument{localEnvs}},
		{name: "remove", about: "delete a snapshot", args: []argument{localEnvs, snapshotNames(false)}},
	}},
	{name: "rollback", about: "put the data of a snapshot back", args: []argument{awakeEnvs, snapshotNames(true)},
		flags: []flagSpec{valued("--only", oneOf("workspace", "home")), plain("--no-snapshot"), yesFlag}},
	{name: "export", about: "write a snapshot to a file of your choosing", args: []argument{awakeEnvs, fileNames}},
	{name: "import", about: "make a new environment from a snapshot file", args: []argument{fileNames, nil}},
	{name: "archive", about: "take a snapshot, then remove containers and volumes", args: []argument{awakeEnvs}, flags: []flagSpec{yesFlag}},
	{name: "unarchive", about: "bring an archived environment back", args: []argument{archivedEnvs}},
	{name: "reset", about: "take the tools back to the image, or the home directory too", args: []argument{awakeEnvs},
		flags: []flagSpec{plain("--home"), plain("--no-snapshot"), yesFlag}},
	{name: "destroy", about: "remove the environment and its data", args: []argument{anyEnvs}, flags: []flagSpec{yesFlag}},
	{name: "migrate", about: "take over an environment of the TypeScript version", args: []argument{nil},
		flags: []flagSpec{yesFlag, plain("--undo")}},
	{name: "version", about: "the build, and the Docker Engine bh talks to"},
	{name: "completion", about: "print a shell completion script", args: []argument{oneOf(completionShells...)}},
	{name: "help", about: "how bh, or one command, is used", args: []argument{helpTopics}},
}

// topLevelFlags are offered for a dash where the command goes. The short
// forms -V and -h work and are not offered: nobody completes two letters.
var topLevelFlags = []string{"--version", "--help"}

func specNamed(specs []commandSpec, name string) *commandSpec {
	for i := range specs {
		if specs[i].name == name {
			return &specs[i]
		}
	}
	return nil
}

func commandCandidates(specs []commandSpec) []candidate {
	out := make([]candidate, len(specs))
	for i, spec := range specs {
		out[i] = candidate{spec.name, spec.about}
	}
	return out
}

// complete is what there is to offer for the last of the words — the one
// under the cursor, as far as it is typed — given the ones before it.
// Candidates that do not start with what is typed are left out.
func (a *app) complete(line []string) []candidate {
	if len(line) == 0 {
		return nil
	}
	current, before := line[len(line)-1], line[:len(line)-1]
	list := a.candidates(current, before)
	if len(list) == 1 && list[0].name == fileNamesMark[0].name {
		return list
	}
	var out []candidate
	for _, entry := range list {
		if strings.HasPrefix(entry.name, current) {
			out = append(out, entry)
		}
	}
	return out
}

func (a *app) candidates(current string, before []string) []candidate {
	if len(before) == 0 {
		if strings.HasPrefix(current, "-") {
			return oneOf(topLevelFlags...)(nil)
		}
		return commandCandidates(completedCommands)
	}
	spec := specNamed(completedCommands, before[0])
	if spec == nil {
		return nil
	}
	rest := before[1:]
	if len(spec.subs) > 0 {
		if len(rest) == 0 {
			return commandCandidates(spec.subs)
		}
		if spec = specNamed(spec.subs, rest[0]); spec == nil {
			return nil
		}
		rest = rest[1:]
	}

	// The words before the cursor: flags (and the values they take) apart
	// from the positions.
	c := &completing{a: a}
	var pending *flagSpec // a flag whose value is the word under the cursor
	used := map[string]bool{}
	for i := 0; i < len(rest); i++ {
		word := rest[i]
		if !strings.HasPrefix(word, "-") || word == "-" {
			c.given = append(c.given, word)
			continue
		}
		name, _, joined := strings.Cut(word, "=")
		used[name] = true
		at := slices.IndexFunc(spec.flags, func(f flagSpec) bool { return f.name == name })
		if at < 0 || !spec.flags[at].takes || joined {
			continue
		}
		if i == len(rest)-1 {
			pending = &spec.flags[at]
		}
		i++ // its value
	}
	switch {
	case pending != nil:
		if pending.values == nil {
			return nil
		}
		return pending.values(c)
	case strings.HasPrefix(current, "-"):
		var out []candidate
		for _, f := range spec.flags {
			if !used[f.name] {
				out = append(out, candidate{name: f.name})
			}
		}
		return out
	case len(c.given) < len(spec.args) && spec.args[len(c.given)] != nil:
		return spec.args[len(c.given)](c)
	}
	return nil
}

// completeLine is `bh __complete -- <words...>`: what the scripts call
// while the user is typing. The words are the command line after "bh", up
// to and including the one under the cursor (empty when the cursor is
// after a space). It prints one candidate a line, "name<TAB>description",
// or the single line "__files__" where a file's name goes. It answers from
// the configuration directory alone — no Docker, no credential store, so
// it is immediate — and on any problem it prints nothing: an error message
// in the middle of a command line being typed helps nobody. It is not in
// the usage text; it is the scripts' business.
func (a *app) completeLine(_ context.Context, args []string) error {
	// --new-word: the cursor is after a space, so the word to complete is
	// an empty one. A shell that cannot pass an empty argument to a
	// program (older PowerShell) says it this way.
	newWord := false
	if len(args) > 0 && args[0] == "--new-word" {
		newWord, args = true, args[1:]
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if newWord {
		args = append(args, "")
	}
	for _, entry := range a.complete(args) {
		switch {
		case entry.name == fileNamesMark[0].name:
			fmt.Fprintln(a.stdout, "__files__")
		case entry.about == "":
			fmt.Fprintln(a.stdout, entry.name)
		default:
			fmt.Fprintf(a.stdout, "%s\t%s\n", entry.name, strings.NewReplacer("\t", " ", "\n", " ").Replace(entry.about))
		}
	}
	return nil
}

// completionShells are the shells bh has a script for.
var completionShells = []string{"zsh", "bash", "powershell"}

func (a *app) completion(_ context.Context, args []string) error {
	if len(args) != 1 {
		return usagef("usage: bh completion %s", strings.Join(completionShells, "|"))
	}
	script, err := completionScript(args[0], a.program)
	if err != nil {
		return err
	}
	fmt.Fprint(a.stdout, script)
	return nil
}

// A program name as the scripts can carry it: letters, digits, dot, dash
// and underscore.
var programName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// completionScript is the script for the shell. It registers itself for
// "bh" and, when this program runs under another name (bh-darwin-arm64,
// before it is installed as bh), for that name too. Whichever name is
// typed is the program the script asks.
func completionScript(shell, program string) (string, error) {
	program = strings.TrimSuffix(program, ".exe")
	names := []string{"bh"}
	if program != "bh" && programName.MatchString(program) {
		names = append(names, program)
	}
	switch shell {
	case "zsh":
		return strings.ReplaceAll(zshCompletion, "{names}", strings.Join(names, " ")), nil
	case "bash":
		return strings.ReplaceAll(bashCompletion, "{names}", strings.Join(names, " ")), nil
	case "powershell", "pwsh":
		return strings.ReplaceAll(powershellCompletion, "{names}", strings.Join(names, ", ")), nil
	}
	return "", usagef("completion: %q is not a supported shell (%s)", shell, strings.Join(completionShells, ", "))
}

// The scripts only carry the command line to bh and its answer back. zsh
// and PowerShell show the descriptions; bash has no place for them.
// The zsh script is used in two ways: evaluated from ~/.zshrc, and as the
// file _bh of a package (share/zsh/site-functions), which zsh loads by its
// first line and runs as the function's first call.
const zshCompletion = `#compdef {names}
# bh completion (zsh) — add to your ~/.zshrc:
#   eval "$(bh completion zsh)"
# (not needed where bh was installed as a package that brings this file)

_bh() {
  local -a lines described bare
  local line name
  lines=("${(@f)$(command "${words[1]}" __complete -- "${(@)words[2,CURRENT]}" 2>/dev/null)}")
  if [[ ${lines[1]} == __files__ ]]; then
    _files
    return
  fi
  for line in $lines; do
    [[ -n $line ]] || continue
    if [[ $line == *$'\t'* ]]; then
      name=${line%%$'\t'*}
      described+=("${name//:/\\:}:${line#*$'\t'}")
    else
      bare+=("$line")
    fi
  done
  # -V: in the order bh gave them (presets go from the least to the most).
  (( ${#described} )) && _describe -V -t values 'bh' described
  (( ${#bare} )) && compadd -V values -- $bare
  (( ${#described} + ${#bare} ))
}

if [[ ${funcstack[1]} == _bh ]]; then
  # Loaded as the file _bh: this is the first completion itself.
  _bh "$@"
else
  compdef _bh {names}
fi
`

const bashCompletion = `# bh completion (bash) — add to your ~/.bashrc:
#   eval "$(bh completion bash)"

_bh() {
  # The words up to the cursor, taken while IFS still has its space: bash 3.2
  # (macOS) makes one word of a slice of an array when it has none.
  local -a words lines
  words=("${COMP_WORDS[@]:1:COMP_CWORD}")
  local IFS=$'\n'
  lines=($(command "${COMP_WORDS[0]}" __complete -- "${words[@]}" 2>/dev/null))
  COMPREPLY=()
  if [[ ${lines[0]} == __files__ ]]; then
    compopt -o filenames 2>/dev/null
    COMPREPLY=($(compgen -f -- "${COMP_WORDS[COMP_CWORD]}"))
    return
  fi
  # In the order bh gave them (presets go from the least to the most);
  # bash before 4.4 cannot, and sorts them.
  compopt -o nosort 2>/dev/null
  local line
  for line in "${lines[@]}"; do
    COMPREPLY+=("${line%%$'\t'*}")
  done
}

complete -F _bh {names}
`

// PowerShell (5.1 and later). An empty argument does not reach a program
// from every version of it, so "the cursor is after a space" travels as
// --new-word. Where a file's name goes the script returns nothing, which
// is what makes PowerShell complete file names. It does the same where bh
// has nothing to offer (a new name, a value): returning an empty result
// to hold PowerShell back makes 7.6 throw, and file names there do no
// harm.
const powershellCompletion = `# bh completion (PowerShell) — add to your $PROFILE:
#   bh completion powershell | Out-String | Invoke-Expression

Register-ArgumentCompleter -Native -CommandName {names} -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $elements = @($commandAst.CommandElements)
    $program = $elements[0].Extent.Text
    $given = @()
    foreach ($element in ($elements | Select-Object -Skip 1)) {
        if ($element.Extent.EndOffset -gt $cursorPosition) { break }
        if ($element -is [System.Management.Automation.Language.StringConstantExpressionAst]) {
            $given += $element.Value
        } else {
            $given += $element.Extent.Text
        }
    }
    $mark = @()
    if ("$wordToComplete" -eq '') { $mark = @('--new-word') }
    $encoding = [Console]::OutputEncoding
    try {
        [Console]::OutputEncoding = [System.Text.Encoding]::UTF8
        $lines = @(& $program __complete @mark -- @given 2>$null)
    } finally {
        [Console]::OutputEncoding = $encoding
    }
    if ($lines.Count -eq 1 -and $lines[0] -eq '__files__') { return }
    foreach ($line in $lines) {
        if (-not $line) { continue }
        $name, $about = $line -split "` + "`" + `t", 2
        if (-not $about) { $about = $name }
        [System.Management.Automation.CompletionResult]::new($name, $name, 'ParameterValue', $about)
    }
}
`
